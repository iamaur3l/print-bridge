package queue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/printer"
)

func TestQueueSuccessfulJob(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	var printCount int32
	mockPrint := func(printerName string, data []byte, jobName string) error {
		atomic.AddInt32(&printCount, 1)
		return nil
	}

	q, err := NewQueue(store, WithPrintFunc(mockPrint), WithMaxAttempts(3))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	sub := q.Subscribe()
	defer q.Unsubscribe(sub)

	job, err := q.Enqueue("ThermalPrinter", []byte("Hello World"), "Test Receipt")
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	finalJob, err := q.GetJobStatus(job.ID)
	if err != nil {
		t.Fatalf("failed to get job status: %v", err)
	}

	if finalJob.Status != StatusSuccess {
		t.Errorf("expected job status %s, got %s (error: %s)", StatusSuccess, finalJob.Status, finalJob.ErrorMessage)
	}

	if atomic.LoadInt32(&printCount) != 1 {
		t.Errorf("expected 1 print attempt, got %d", printCount)
	}
}

func TestQueueRetryFailureJob(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	var printAttempts int32
	mockPrint := func(printerName string, data []byte, jobName string) error {
		atomic.AddInt32(&printAttempts, 1)
		return errors.New("printer paper out")
	}

	q, err := NewQueue(store, WithPrintFunc(mockPrint), WithMaxAttempts(2))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	job, err := q.Enqueue("JamPrinter", []byte("Test Payload"), "Failure Test")
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// Wait for retry attempts to complete
	time.Sleep(3500 * time.Millisecond)

	finalJob, err := q.GetJobStatus(job.ID)
	if err != nil {
		t.Fatalf("failed to get job status: %v", err)
	}

	if finalJob.Status != StatusFailed {
		t.Errorf("expected job status %s, got %s", StatusFailed, finalJob.Status)
	}

	if atomic.LoadInt32(&printAttempts) != 2 {
		t.Errorf("expected 2 attempts before marking failed, got %d", printAttempts)
	}
}

func TestStoreRetentionCleanup(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	job := &Job{
		ID:          "old-job-1",
		PrinterName: "TestPrinter",
		JobName:     "Old Job",
		Payload:     []byte("data"),
		Status:      StatusSuccess,
		CreatedAt:   time.Now().AddDate(0, 0, -10), // 10 days old
		Attempts:    1,
		MaxAttempts: 5,
	}
	if err := store.CreateJob(job); err != nil {
		t.Fatalf("failed to create old job: %v", err)
	}

	cleaned, err := store.CleanOldJobs(7)
	if err != nil {
		t.Fatalf("CleanOldJobs failed: %v", err)
	}

	if cleaned != 1 {
		t.Errorf("expected 1 job cleaned, got %d", cleaned)
	}

	_, err = store.GetJob("old-job-1")
	if err == nil {
		t.Errorf("expected old job to be deleted")
	}
}

func TestPrinterBindingPersistence(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	bindings, err := store.ListPrinterBindings()
	if err != nil {
		t.Fatalf("ListPrinterBindings failed: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("expected no bindings initially, got %+v", bindings)
	}

	// A binding needs both a queue name and a port.
	if err := store.SavePrinterBinding(printer.PortBinding{PrinterName: "Receipt"}); err == nil {
		t.Error("expected an error when the port name is missing")
	}

	first := printer.PortBinding{PrinterName: "Receipt", PortName: "USB003", DeviceID: "VID_04B8&PID_0E15"}
	if err := store.SavePrinterBinding(first); err != nil {
		t.Fatalf("SavePrinterBinding failed: %v", err)
	}

	// Moving the cable must update the same row rather than accumulating rows.
	moved := printer.PortBinding{PrinterName: "Receipt", PortName: "USB011", DeviceID: first.DeviceID}
	if err := store.SavePrinterBinding(moved); err != nil {
		t.Fatalf("SavePrinterBinding failed: %v", err)
	}

	bindings, err = store.ListPrinterBindings()
	if err != nil {
		t.Fatalf("ListPrinterBindings failed: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected exactly one binding per queue, got %+v", bindings)
	}
	if bindings[0].PortName != "USB011" || bindings[0].DeviceID != first.DeviceID {
		t.Errorf("expected the binding to be updated in place, got %+v", bindings[0])
	}
}

// TestMigrateAddsColumnsToLegacyDatabase covers the upgrade path that runs on a
// customer's machine: a database written by an older agent version must gain the
// new columns in place, without losing data.
func TestMigrateAddsColumnsToLegacyDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to create legacy database: %v", err)
	}

	// The printer_telemetry and jobs tables as earlier versions created them.
	if _, err := legacy.Exec(`
	CREATE TABLE printer_telemetry (
		printer_name TEXT PRIMARY KEY,
		driver_name TEXT,
		port_name TEXT,
		is_online INTEGER NOT NULL,
		status_description TEXT,
		last_seen_at DATETIME NOT NULL
	);`); err != nil {
		t.Fatalf("failed to create legacy schema: %v", err)
	}

	// A legacy jobs table has none of priority / idempotency_key / next_attempt_at,
	// which is exactly the case that breaks a naive migration.
	if _, err := legacy.Exec(`
	CREATE TABLE jobs (
		id TEXT PRIMARY KEY,
		printer_name TEXT NOT NULL,
		job_name TEXT NOT NULL,
		payload BLOB NOT NULL,
		status TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		last_attempt_at DATETIME,
		attempts INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 5,
		error_message TEXT
	);`); err != nil {
		t.Fatalf("failed to create legacy jobs schema: %v", err)
	}
	if _, err := legacy.Exec(`
	INSERT INTO jobs (id, printer_name, job_name, payload, status, created_at, attempts, max_attempts)
	VALUES ('legacy-job', 'Legacy Printer', 'Legacy Job', X'1B40', 'success', '2026-01-01T00:00:00Z', 1, 5);`); err != nil {
		t.Fatalf("failed to seed legacy job: %v", err)
	}

	if _, err := legacy.Exec(`
	INSERT INTO printer_telemetry (printer_name, driver_name, port_name, is_online, status_description, last_seen_at)
	VALUES ('Legacy Printer', 'Generic', 'USB001', 1, 'Ready', '2026-01-01T00:00:00Z');`); err != nil {
		t.Fatalf("failed to seed legacy row: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("failed to close legacy database: %v", err)
	}

	// Reopening through NewStore must migrate in place.
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed on a legacy database: %v", err)
	}
	defer store.Close()

	// The pre-existing row survives...
	var name string
	if err := store.DB().QueryRow(`SELECT printer_name FROM printer_telemetry`).Scan(&name); err != nil {
		t.Fatalf("legacy row was lost during migration: %v", err)
	}
	if name != "Legacy Printer" {
		t.Errorf("expected the legacy row to be preserved, got %q", name)
	}

	// ...and the new columns are usable.
	if _, err := store.DB().Exec(`
	INSERT INTO printer_telemetry (printer_name, driver_name, port_name, is_online, state, status_description, status_detail, last_seen_at)
	VALUES ('New Printer', 'Generic', 'USB002', 1, 'online', 'Ready', 'verified', '2026-01-01T00:00:00Z');`); err != nil {
		t.Fatalf("migrated database is missing the new columns: %v", err)
	}

	// Migrations must be repeatable: reopening again must not fail.
	store2, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("second migration run failed: %v", err)
	}
	_ = store2.Close()

	counts, err := store.CountJobsByStatus()
	if err != nil {
		t.Fatalf("CountJobsByStatus failed on a migrated database: %v", err)
	}
	if counts.Total != 1 {
		t.Errorf("expected the migrated legacy job to be preserved, got %+v", counts)
	}

	// The legacy job survived, and the queue queries that touch the new columns work.
	legacyJob, err := store.GetJob("legacy-job")
	if err != nil {
		t.Fatalf("legacy job was lost during migration: %v", err)
	}
	if legacyJob.Status != StatusSuccess || legacyJob.Priority != 0 {
		t.Errorf("unexpected migrated job: %+v", legacyJob)
	}
	if _, err := store.GetPendingJobs(); err != nil {
		t.Fatalf("GetPendingJobs failed on a migrated database: %v", err)
	}

	// The new columns are usable...
	if err := store.CreateJob(&Job{
		ID:             "new-job",
		PrinterName:    "Printer",
		JobName:        "New",
		Payload:        []byte("x"),
		Status:         StatusQueued,
		CreatedAt:      time.Now(),
		MaxAttempts:    3,
		Priority:       5,
		IdempotencyKey: "order-legacy",
	}); err != nil {
		t.Fatalf("creating a job with the new columns failed: %v", err)
	}

	// ...including the partial unique index on the idempotency key.
	if err := store.CreateJob(&Job{
		ID:             "dup-job",
		PrinterName:    "Printer",
		JobName:        "Dup",
		Payload:        []byte("x"),
		Status:         StatusQueued,
		CreatedAt:      time.Now(),
		MaxAttempts:    3,
		IdempotencyKey: "order-legacy",
	}); err == nil {
		t.Error("expected the idempotency key index to reject a duplicate")
	}
}
