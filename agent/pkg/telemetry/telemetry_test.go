package telemetry

import (
	"testing"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

func TestTelemetryMonitorEvents(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	mon := NewMonitor(store, 1*time.Second)

	mockStatus := "Ready"
	mockOnline := true
	mon.SetListFunc(func() ([]printer.PrinterInfo, error) {
		return []printer.PrinterInfo{
			{
				Name:              "TestReceiptPrinter",
				DriverName:        "Generic Driver",
				PortName:          "USB001",
				IsOnline:          mockOnline,
				StatusDescription: mockStatus,
			},
		}, nil
	})

	sub := mon.Subscribe()
	defer mon.Unsubscribe(sub)

	// Initial poll
	mon.PollOnce()

	select {
	case evt := <-sub:
		if evt.Printer.PrinterName != "TestReceiptPrinter" || !evt.Printer.IsOnline {
			t.Errorf("unexpected initial telemetry event: %+v", evt)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timeout waiting for initial telemetry event")
	}

	// State change poll (simulating paper out failure)
	mockStatus = "Paper Out"
	mockOnline = false
	mon.PollOnce()

	select {
	case evt := <-sub:
		if evt.Printer.StatusDescription != "Paper Out" || evt.Printer.IsOnline {
			t.Errorf("unexpected state change event: %+v", evt)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timeout waiting for state change event")
	}

	// Verify SQLite persistence via GetHealth()
	health, err := mon.GetHealth()
	if err != nil {
		t.Fatalf("GetHealth failed: %v", err)
	}

	if len(health) != 1 || health[0].StatusDescription != "Paper Out" {
		t.Errorf("unexpected health from DB: %+v", health)
	}
}
