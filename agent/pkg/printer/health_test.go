package printer

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEvaluateStatusIgnoresStaleWorkOfflineFlag is the regression test for the
// worst failure this project had: a USB thermal printer that works perfectly but
// reports WorkOffline: true, which the naive check turned into "Offline".
func TestEvaluateStatusIgnoresStaleWorkOfflineFlag(t *testing.T) {
	now := time.Now()
	verdict := EvaluateStatus(StatusEvidence{
		WorkOfflineAttr: true,
		OfflineBit:      true,
	}, now)

	if verdict.State == StateOffline {
		t.Error("a stale spooler flag must never produce an offline verdict")
	}
	if verdict.State != StateUnknown {
		t.Errorf("expected state %q for an uncorroborated offline flag, got %q", StateUnknown, verdict.State)
	}
	if !verdict.StaleFlags {
		t.Error("expected StaleFlags so the UI can explain the contradiction")
	}
	if verdict.Detail == "" {
		t.Error("expected a StatusDetail explaining the verdict")
	}
}

func TestEvaluateStatusFaultsAreOffline(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name string
		ev   StatusEvidence
		want string
	}{
		{"paper jam", StatusEvidence{PaperJam: true}, "Paper jam"},
		{"paper out", StatusEvidence{PaperOut: true}, "Paper out"},
		{"door open", StatusEvidence{DoorOpen: true}, "Door open"},
		{"no toner", StatusEvidence{NoToner: true}, "Out of toner"},
		{"user intervention", StatusEvidence{UserIntervention: true}, "User intervention required"},
		{"error bit", StatusEvidence{ErrorBit: true}, "Error"},
		{"printer not available", StatusEvidence{NotAvailable: true}, "Not available"},
		{"paused queue", StatusEvidence{Paused: true}, "Paused"},
		{"port absent", StatusEvidence{PortAbsent: true}, "Port not present"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := EvaluateStatus(tc.ev, now)
			if verdict.State != StateOffline {
				t.Fatalf("expected offline, got %q (detail: %s)", verdict.State, verdict.Detail)
			}
			if verdict.Description != tc.want {
				t.Errorf("expected description %q, got %q", tc.want, verdict.Description)
			}
		})
	}
}

func TestEvaluateStatusUnknownCodesAreReady(t *testing.T) {
	// A printer reporting nothing we recognise (status code 1 "Other" or
	// 2 "Unknown" on cheap thermal units) must be ready, never an error.
	verdict := EvaluateStatus(StatusEvidence{}, time.Now())
	if verdict.State != StateOnline {
		t.Errorf("expected online for absent/unknown status codes, got %q", verdict.State)
	}
	if verdict.Description != "Ready" {
		t.Errorf("expected description %q, got %q", "Ready", verdict.Description)
	}
}

func TestEvaluateStatusWriteIsGroundTruth(t *testing.T) {
	now := time.Now()

	verdict := EvaluateStatus(StatusEvidence{
		WorkOfflineAttr: true,
		OfflineBit:      true,
		LastWriteOK:     now.Add(-10 * time.Second),
	}, now)
	if verdict.State != StateOnline {
		t.Fatalf("a completed write must override spooler flags, got %q", verdict.State)
	}
	if !verdict.StaleFlags {
		t.Error("expected the contradicted spooler flags to be reported as stale")
	}

	// ...but only inside the trust window.
	expired := EvaluateStatus(StatusEvidence{
		WorkOfflineAttr: true,
		OfflineBit:      true,
		LastWriteOK:     now.Add(-2 * WriteTrustWindow),
	}, now)
	if expired.State != StateUnknown {
		t.Errorf("expected the write trust window to expire, got %q", expired.State)
	}

	// A real fault still wins over a recent write: WritePrinter succeeds into the
	// spooler even when the device cannot print.
	faulted := EvaluateStatus(StatusEvidence{PaperOut: true, LastWriteOK: now.Add(-5 * time.Second)}, now)
	if faulted.State != StateOffline {
		t.Errorf("expected a paper-out fault to stay offline, got %q", faulted.State)
	}
}

func TestRecordSuccessfulWrite(t *testing.T) {
	const name = "PrintBridge Test Printer (write tracker)"

	if !LastSuccessfulWrite(name).IsZero() {
		t.Fatal("expected no recorded write for a fresh printer")
	}

	RecordSuccessfulWrite(name)
	if LastSuccessfulWrite(name).IsZero() {
		t.Fatal("expected a recorded write timestamp")
	}

	RecordSuccessfulWrite("")
	if !LastSuccessfulWrite("").IsZero() {
		t.Error("expected an empty printer name to be ignored")
	}
}

func TestSnapshotterCachesUntilInvalidated(t *testing.T) {
	var calls int32
	snap := NewSnapshotter(time.Hour, func() ([]PrinterInfo, error) {
		atomic.AddInt32(&calls, 1)
		return []PrinterInfo{{Name: "Cached Printer", IsOnline: true}}, nil
	})

	for i := 0; i < 5; i++ {
		list, err := snap.List()
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		if len(list) != 1 || list[0].Name != "Cached Printer" {
			t.Fatalf("unexpected snapshot: %+v", list)
		}
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 enumeration for 5 reads inside the TTL, got %d", got)
	}

	// A caller mutating its copy must not corrupt the cached snapshot.
	list, _ := snap.List()
	list[0].Name = "Mutated"
	again, _ := snap.List()
	if again[0].Name != "Cached Printer" {
		t.Error("returned snapshot aliases the cache")
	}

	snap.Invalidate()
	if _, err := snap.List(); err != nil {
		t.Fatalf("List after Invalidate failed: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected Invalidate to force a re-enumeration, got %d calls", got)
	}
}

// TestSnapshotterSingleFlight checks that a burst of callers collapses into one
// enumeration, which is what keeps a health sweep across N printers cheap.
func TestSnapshotterSingleFlight(t *testing.T) {
	var calls int32
	release := make(chan struct{})

	snap := NewSnapshotter(time.Millisecond, func() ([]PrinterInfo, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return []PrinterInfo{{Name: "Slow Printer"}}, nil
	})

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			if _, err := snap.List(); err != nil {
				t.Errorf("List failed: %v", err)
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected concurrent callers to share one enumeration, got %d", got)
	}
}