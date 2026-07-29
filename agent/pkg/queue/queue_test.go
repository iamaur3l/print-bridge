package queue

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
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
