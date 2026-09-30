package relay

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/queue"
)

// frameLog records everything the agent sends to a mock relay.
type frameLog struct {
	mu     sync.Mutex
	frames []map[string]any
}

func (f *frameLog) add(frame map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, frame)
}

func (f *frameLog) find(frameType, stage string) (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, frame := range f.frames {
		if frame["type"] != frameType {
			continue
		}
		if stage != "" && frame["stage"] != stage {
			continue
		}
		return frame, true
	}
	return nil, false
}

func (f *frameLog) waitFor(t *testing.T, frameType, stage string, timeout time.Duration) map[string]any {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if frame, found := f.find(frameType, stage); found {
			return frame
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for a %q frame with stage %q", frameType, stage)
	return nil
}

// TestRelayClientAcknowledgesCloudJobs drives a cloud job through a client whose printer
// always fails, and checks the two acknowledgements the relay relies on: accepted (with
// the local job id) and completed (with the outcome).
func TestRelayClientAcknowledgesCloudJobs(t *testing.T) {
	frames := &frameLog{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/agent/agent_ack/ws" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		frames.add(map[string]any{"type": "handshake", "authorization": r.Header.Get("Authorization")})

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		if err := ws.WriteJSON(map[string]any{
			"version": 1,
			"id":      "relay_job_1",
			"type":    "print",
			"payload": map[string]any{
				"printer":  "BackOffice",
				"data":     base64.StdEncoding.EncodeToString([]byte("out of paper test")),
				"job_name": "Cloud Job",
			},
		}); err != nil {
			return
		}

		for {
			_, message, err := ws.ReadMessage()
			if err != nil {
				return
			}

			var frame map[string]any
			if json.Unmarshal(message, &frame) == nil {
				frames.add(frame)
			}
		}
	}))
	defer server.Close()

	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	// A printer that always fails sends the job straight to a terminal state, which is
	// what the completion acknowledgement reports.
	q, err := queue.NewQueue(store,
		queue.WithPrintFunc(func(string, []byte, string) error {
			return errors.New("printer is out of paper")
		}),
		queue.WithMaxAttempts(1),
	)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	client := NewClient(strings.Replace(server.URL, "http://", "ws://", 1), "agent_ack", "agent_secret", q)
	client.SetVersion("9.9.9-test")
	client.Start()
	defer client.Stop()

	// The agent announces itself and its version so the relay's status endpoint can
	// report what is on the other end.
	hello := frames.waitFor(t, "hello", "", 5*time.Second)
	if hello["agent_id"] != "agent_ack" || hello["version"] != "9.9.9-test" {
		t.Errorf("unexpected hello frame: %+v", hello)
	}

	handshake, _ := frames.find("handshake", "")
	if handshake["authorization"] != "Bearer agent_secret" {
		t.Errorf("the agent secret must be presented on the upgrade, got %q", handshake["authorization"])
	}

	accepted := frames.waitFor(t, "job_result", "accepted", 5*time.Second)
	if accepted["id"] != "relay_job_1" {
		t.Errorf("accepted acknowledgement is for the wrong job: %+v", accepted)
	}
	if accepted["status"] != "accepted" {
		t.Errorf("unexpected accepted status: %+v", accepted)
	}
	localJobID, _ := accepted["local_job_id"].(string)
	if localJobID == "" {
		t.Fatalf("accepted acknowledgement must carry the local job id: %+v", accepted)
	}

	completed := frames.waitFor(t, "job_result", "completed", 10*time.Second)
	if completed["id"] != "relay_job_1" {
		t.Errorf("completion is for the wrong job: %+v", completed)
	}
	if completed["status"] != "failed" {
		t.Errorf("a job that could not be printed must report failure: %+v", completed)
	}
	if completed["local_job_id"] != localJobID {
		t.Errorf("completion must reference the same local job (%v), got %+v", localJobID, completed)
	}
	if errorText, _ := completed["error"].(string); !strings.Contains(errorText, "out of paper") {
		t.Errorf("completion should carry the printer's error, got %q", errorText)
	}
}

// TestRelayClientSurfacesRelayRefusal checks that a rejected connection explains itself:
// the relay answers 401/503 with JSON, and that text is more use in the agent log than
// gorilla's bare "bad handshake".
func TestRelayClientSurfacesRelayRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	q, err := queue.NewQueue(store, queue.WithPrintFunc(func(string, []byte, string) error { return nil }))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	client := NewClient(strings.Replace(server.URL, "http://", "ws://", 1), "agent_123", "wrong_secret", q)

	connected, err := client.connectAndServe()
	if connected {
		t.Error("a refused connection must not be reported as connected")
	}
	if err == nil {
		t.Fatal("expected an error for a refused connection")
	}
	if !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("the refusal should explain itself, got: %v", err)
	}
}