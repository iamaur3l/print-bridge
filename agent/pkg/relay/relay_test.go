package relay

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

var upgrader = websocket.Upgrader{}

func TestRelayClientOutbound(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	q, err := queue.NewQueue(store, queue.WithPrintFunc(func(printerName string, data []byte, jobName string) error {
		return nil
	}))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	// Mock Cloud Relay Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		// Send mock remote print job down to agent client
		printData := base64.StdEncoding.EncodeToString([]byte("Cloud Print Data"))
		req := map[string]any{
			"version": 1,
			"id":      "remote_job_101",
			"type":    "print",
			"payload": map[string]any{
				"printer":  "ThermalCloudPrinter",
				"data":     printData,
				"job_name": "Cloud Job #1",
			},
		}

		_ = ws.WriteJSON(req)
		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	relayURL := strings.Replace(server.URL, "http://", "ws://", 1)

	client := NewClient(relayURL, "agent_123", "secret_key", q)
	client.Start()

	time.Sleep(600 * time.Millisecond)
	client.Stop()

	jobs, err := store.ListRecentJobs(10)
	if err != nil {
		t.Fatalf("failed to list recent jobs: %v", err)
	}

	if len(jobs) != 1 || jobs[0].PrinterName != "ThermalCloudPrinter" {
		t.Errorf("expected 1 cloud print job enqueued, got: %+v", jobs)
	}
}
