package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

// RelayClient keeps one outbound WebSocket to the PrintBridge Cloud Relay, so a backend
// can print to a till that is not reachable from the internet.
//
// The connection authenticates with the agent's own secret (the relay stores only its
// SHA-256), is kept alive with pings, and every job the relay hands over is acknowledged
// twice: once when it has been accepted into the local queue, and once when it has
// finished printing. Without the second acknowledgement a backend could never tell the
// difference between "the till printed the receipt" and "the till took the job and the
// printer was out of paper".
type RelayClient struct {
	relayURL string
	agentID  string
	apiKey   string
	version  string
	queue    *queue.Queue

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	connMu sync.Mutex
	conn   *websocket.Conn
	// gorilla/websocket permits one writer at a time, and acknowledgements, pings and
	// the handshake can all write.
	writeMu sync.Mutex

	// Jobs awaiting a completion acknowledgement, keyed by the local job id.
	trackedMu sync.Mutex
	tracked   map[string]trackedJob
}

type trackedJob struct {
	relayJobID string
	printer    string
	started    time.Time
}

const (
	relayPingInterval = 30 * time.Second
	// A silent relay must not leave the agent waiting forever on a dead socket.
	relayPongTimeout = 90 * time.Second
	// How long to keep waiting for a local job to finish before giving up on acking it.
	relayAckTTL     = 10 * time.Minute
	relayMinBackoff = 3 * time.Second
	relayMaxBackoff = 60 * time.Second
)

func NewClient(relayURL, agentID, apiKey string, q *queue.Queue) *RelayClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &RelayClient{
		relayURL: relayURL,
		agentID:  agentID,
		apiKey:   apiKey,
		queue:    q,
		ctx:      ctx,
		cancel:   cancel,
		tracked:  make(map[string]trackedJob),
	}
}

// SetVersion records the agent version reported to the relay when it connects.
func (c *RelayClient) SetVersion(version string) {
	c.version = version
}

func (c *RelayClient) Start() {
	c.wg.Add(2)
	go c.loop()
	go c.watchJobs()
}

func (c *RelayClient) Stop() {
	c.cancel()
	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.connMu.Unlock()
	c.wg.Wait()
}

func (c *RelayClient) loop() {
	defer c.wg.Done()

	backoff := relayMinBackoff
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		connected, err := c.connectAndServe()
		if c.ctx.Err() != nil {
			return
		}

		// A connection that worked retries at the base delay; one that was refused
		// backs off, so a wrong secret does not hammer the relay twice a second.
		if connected {
			backoff = relayMinBackoff
		} else {
			backoff *= 2
			if backoff > relayMaxBackoff {
				backoff = relayMaxBackoff
			}
		}

		log.Printf("[Relay] Cloud Relay socket closed (%v). Reconnecting in %v...", err, backoff)

		select {
		case <-time.After(backoff):
		case <-c.ctx.Done():
			return
		}
	}
}

// connectAndServe dials the relay, announces the agent, and reads until the socket dies.
// It reports whether the connection was ever established, which the caller uses to decide
// how hard to back off.
func (c *RelayClient) connectAndServe() (bool, error) {
	wsURL := fmt.Sprintf("%s/ws/agent/%s/ws", c.relayURL, url.PathEscape(c.agentID))
	headers := http.Header{}
	if c.apiKey != "" {
		headers.Set("Authorization", "Bearer "+c.apiKey)
	}

	log.Printf("[Relay] Connecting outbound Cloud Relay socket to %s", wsURL)

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		// The relay explains refusals in JSON, so surface that instead of a bare "bad
		// handshake" that leaves the operator guessing.
		if resp != nil && resp.StatusCode >= 400 {
			return false, fmt.Errorf("relay refused the connection: HTTP %d (%s) — check relay.agent_id and relay.api_key",
				resp.StatusCode, describeRelayError(resp))
		}
		return false, fmt.Errorf("failed to dial relay WebSocket: %w", err)
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	defer func() {
		c.connMu.Lock()
		c.conn = nil
		c.connMu.Unlock()
		conn.Close()
	}()

	_ = conn.SetReadDeadline(time.Now().Add(relayPongTimeout))

	log.Printf("[Relay] Outbound Cloud Relay connection active for Agent ID: %s", c.agentID)

	if err := c.send(map[string]any{"type": "hello", "agent_id": c.agentID, "version": c.version}); err != nil {
		return true, err
	}

	connCtx, connCancel := context.WithCancel(c.ctx)
	defer connCancel()
	go c.keepAlive(connCtx)

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return true, err
		}

		// Anything from the relay, a pong included, proves it is still there.
		_ = conn.SetReadDeadline(time.Now().Add(relayPongTimeout))

		var req struct {
			Version int            `json:"version"`
			ID      string         `json:"id"`
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(message, &req); err != nil {
			continue
		}

		if req.Type == "print" {
			c.handleRemoteJob(req.ID, req.Payload)
		}
	}
}

// keepAlive stops the socket being reclaimed by an idle timeout somewhere in the middle,
// and gives the read deadline above something to measure.
func (c *RelayClient) keepAlive(ctx context.Context) {
	ticker := time.NewTicker(relayPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.send(map[string]any{"type": "ping"}); err != nil {
				return // the read loop notices the same failure and reconnects
			}
		}
	}
}

// handleRemoteJob is the cloud print path: decode, queue, acknowledge.
func (c *RelayClient) handleRemoteJob(relayJobID string, payload map[string]any) {
	if relayJobID == "" {
		return
	}

	printerName, _ := payload["printer"].(string)
	dataBase64, _ := payload["data"].(string)
	jobName, _ := payload["job_name"].(string)

	if printerName == "" || dataBase64 == "" {
		c.ackFailure(relayJobID, "cloud job had no printer or no data")
		return
	}

	rawBytes, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		c.ackFailure(relayJobID, fmt.Sprintf("cloud job payload is not valid base64: %v", err))
		return
	}

	job, err := c.queue.Enqueue(printerName, rawBytes, jobName)
	if err != nil {
		c.ackFailure(relayJobID, err.Error())
		return
	}

	c.track(job.ID, relayJobID, printerName)
	log.Printf("[Relay] Received remote cloud job %s -> enqueued local job %s on %q", relayJobID, job.ID, printerName)

	// Accepted: the relay may now stop re-sending it.
	c.sendAck(relayJobID, "accepted", "accepted", job.ID, "")

	// A job can reach a terminal state before the tracker above was registered — an
	// unknown printer fails instantly. Read the current state so that outcome, and the
	// completion acknowledgement that carries it, cannot be missed. If the job already
	// finished, the notification has been handled as untracked and sent nothing; if it
	// finishes later, the notification reports it and this read changes nothing.
	if current, err := c.queue.GetJobStatus(job.ID); err == nil {
		c.reportOutcome(current)
	}
}

// watchJobs reports the outcome of tracked jobs back to the relay.
func (c *RelayClient) watchJobs() {
	defer c.wg.Done()

	updates := c.queue.Subscribe()
	defer c.queue.Unsubscribe(updates)

	for {
		select {
		case <-c.ctx.Done():
			return
		case job, ok := <-updates:
			if !ok {
				return
			}
			c.reportOutcome(job)
		}
	}
}

func (c *RelayClient) reportOutcome(job *queue.Job) {
	if job == nil {
		return
	}

	finished := job.Status == queue.StatusSuccess || job.Status == queue.StatusFailed

	c.trackedMu.Lock()
	tracked, isTracked := c.tracked[job.ID]
	if finished && isTracked {
		delete(c.tracked, job.ID)
	}
	// A job that never reaches a terminal state (the printer was switched off for the
	// day) must not accumulate here forever.
	cutoff := time.Now().Add(-relayAckTTL)
	for localID, entry := range c.tracked {
		if entry.started.Before(cutoff) {
			delete(c.tracked, localID)
		}
	}
	c.trackedMu.Unlock()

	if !finished || !isTracked {
		return
	}

	status := "success"
	if job.Status == queue.StatusFailed {
		status = "failed"
	}

	log.Printf("[Relay] Local job %s for cloud job %s finished: %s", job.ID, tracked.relayJobID, status)
	c.sendAck(tracked.relayJobID, "completed", status, job.ID, job.ErrorMessage)
}

func (c *RelayClient) track(localJobID, relayJobID, printerName string) {
	c.trackedMu.Lock()
	defer c.trackedMu.Unlock()

	c.tracked[localJobID] = trackedJob{relayJobID: relayJobID, printer: printerName, started: time.Now()}
}

func (c *RelayClient) ackFailure(relayJobID, reason string) {
	log.Printf("[Relay] Rejected remote cloud job %s: %s", relayJobID, reason)
	// Reported as an accepted-then-failed result, so a backend learns immediately
	// rather than waiting for a completion that will never come.
	c.sendAck(relayJobID, "accepted", "failed", "", reason)
}

func (c *RelayClient) sendAck(relayJobID, stage, status, localJobID, errorMessage string) {
	message := map[string]any{
		"type":   "job_result",
		"id":     relayJobID,
		"stage":  stage,
		"status": status,
	}
	if localJobID != "" {
		message["local_job_id"] = localJobID
	}
	if errorMessage != "" {
		message["error"] = errorMessage
	}

	if err := c.send(message); err != nil {
		log.Printf("[Relay] Failed to send the %s acknowledgement for cloud job %s: %v", stage, relayJobID, err)
	}
}

// send writes one JSON frame, serialised because gorilla permits a single writer.
func (c *RelayClient) send(message map[string]any) error {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return fmt.Errorf("no relay connection")
	}

	encoded, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to encode relay message: %w", err)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	return conn.WriteMessage(websocket.TextMessage, encoded)
}

// describeRelayError extracts the relay's own explanation from a refused handshake.
func describeRelayError(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return "no response body"
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return "unreadable response body"
	}

	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		return payload.Error
	}

	if text := strings.TrimSpace(string(body)); text != "" {
		return text
	}

	return "no response body"
}
