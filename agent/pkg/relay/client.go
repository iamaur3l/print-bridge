package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

type RelayClient struct {
	relayURL  string
	agentID   string
	apiKey    string
	queue     *queue.Queue
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	conn      *websocket.Conn
	connMu    sync.Mutex
	reconnect time.Duration
}

func NewClient(relayURL, agentID, apiKey string, q *queue.Queue) *RelayClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &RelayClient{
		relayURL:  relayURL,
		agentID:   agentID,
		apiKey:    apiKey,
		queue:     q,
		ctx:       ctx,
		cancel:    cancel,
		reconnect: 3 * time.Second,
	}
}

func (c *RelayClient) Start() {
	c.wg.Add(1)
	go c.loop()
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

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		err := c.connectAndServe()
		if err != nil && c.ctx.Err() == nil {
			log.Printf("[Relay] Cloud Relay socket disconnected: %v. Reconnecting in %v...", err, c.reconnect)
			time.Sleep(c.reconnect)
		}
	}
}

func (c *RelayClient) connectAndServe() error {
	wsURL := fmt.Sprintf("%s/ws/agent/%s/ws", c.relayURL, c.agentID)
	headers := http.Header{}
	if c.apiKey != "" {
		headers.Set("Authorization", "Bearer "+c.apiKey)
	}

	log.Printf("[Relay] Connecting outbound Cloud Relay socket to %s", wsURL)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		return fmt.Errorf("failed to dial relay WebSocket: %w", err)
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

	log.Printf("[Relay] Outbound Cloud Relay connection active for Agent ID: %s", c.agentID)

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return err
		}

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
			printerName, _ := req.Payload["printer"].(string)
			dataBase64, _ := req.Payload["data"].(string)
			jobName, _ := req.Payload["job_name"].(string)

			if printerName != "" && dataBase64 != "" {
				rawBytes, err := base64.StdEncoding.DecodeString(dataBase64)
				if err == nil {
					job, err := c.queue.Enqueue(printerName, rawBytes, jobName)
					if err == nil {
						log.Printf("[Relay] Received remote cloud job %s -> enqueued local job %s", req.ID, job.ID)
					}
				}
			}
		}
	}
}
