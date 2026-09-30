package queue

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestQueueHonoursPriority proves the queue is ordered by priority rather than
// arrival order, which is how an order ticket can overtake a queued report.
func TestQueueHonoursPriority(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	printed := make(chan string, 8)
	q, err := NewQueue(store, WithPrintFunc(func(printerName string, data []byte, jobName string) error {
		printed <- jobName
		return nil
	}))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	// Pause first so all three jobs are queued before anything is dispatched; the
	// ordering decision is then made in one pass.
	q.Pause()

	for _, job := range []struct {
		name     string
		priority int
	}{
		{"normal", 0},
		{"urgent", 5},
		{"elevated", 1},
	} {
		if _, _, err := q.EnqueueJob("Printer", []byte("payload"), EnqueueOptions{
			JobName:  job.name,
			Priority: job.priority,
		}); err != nil {
			t.Fatalf("enqueue %s failed: %v", job.name, err)
		}
	}

	q.Resume()

	want := []string{"urgent", "elevated", "normal"}
	for _, expected := range want {
		select {
		case got := <-printed:
			if got != expected {
				t.Fatalf("expected %q to print next, got %q", expected, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", expected)
		}
	}
}

// TestQueueIdempotentEnqueue proves a retried POS request cannot print twice.
func TestQueueIdempotentEnqueue(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	q, err := NewQueue(store, WithPrintFunc(func(string, []byte, string) error { return nil }))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	opts := EnqueueOptions{JobName: "Receipt #1", IdempotencyKey: "order-1"}

	first, created, err := q.EnqueueJob("Printer", []byte("payload"), opts)
	if err != nil {
		t.Fatalf("first enqueue failed: %v", err)
	}
	if !created {
		t.Error("expected the first enqueue to create a job")
	}

	second, created, err := q.EnqueueJob("Printer", []byte("payload"), opts)
	if err != nil {
		t.Fatalf("second enqueue failed: %v", err)
	}
	if created {
		t.Error("expected the duplicate enqueue to reuse the existing job")
	}
	if second.ID != first.ID {
		t.Errorf("expected the same job ID, got %q then %q", first.ID, second.ID)
	}

	counts, err := store.CountJobsByStatus()
	if err != nil {
		t.Fatalf("CountJobsByStatus failed: %v", err)
	}
	if counts.Total != 1 {
		t.Errorf("expected exactly one job to exist, got %+v", counts)
	}

	// A different key is still a new job.
	if _, created, err := q.EnqueueJob("Printer", []byte("payload"), EnqueueOptions{
		JobName: "Receipt #2", IdempotencyKey: "order-2",
	}); err != nil || !created {
		t.Errorf("expected a different key to create a job (created=%t, err=%v)", created, err)
	}
}

// TestQueueDepthLimitRefusesWork proves the queue refuses rather than growing without
// bound when no printer is working.
func TestQueueDepthLimitRefusesWork(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	q, err := NewQueue(store,
		WithPrintFunc(func(string, []byte, string) error { return nil }),
		WithMaxDepth(2),
	)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	// Paused, so nothing drains and the limit is what stops the third job.
	q.Pause()

	for i := 0; i < 2; i++ {
		if _, err := q.Enqueue("Printer", []byte("payload"), fmt.Sprintf("job-%d", i)); err != nil {
			t.Fatalf("enqueue %d failed: %v", i, err)
		}
	}

	_, err = q.Enqueue("Printer", []byte("payload"), "overflow")
	if !errors.Is(err, ErrQueueFull) {
		t.Errorf("expected ErrQueueFull, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "2 unfinished") {
		t.Errorf("expected the error to explain the depth, got %q", err.Error())
	}
}

// TestQueueHungPrinterDoesNotBlockOthers is the head-of-line regression test. A
// printer whose socket hangs used to stall the single worker loop for the whole
// retry budget, delaying every other station's receipts.
func TestQueueHungPrinterDoesNotBlockOthers(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }

	fastDone := make(chan string, 1)

	q, err := NewQueue(store, WithPrintFunc(func(printerName string, data []byte, jobName string) error {
		if printerName == "Hung" {
			// Stands in for a network printer whose socket never answers.
			<-release
			return errors.New("connection timed out")
		}
		fastDone <- printerName
		return nil
	}))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer func() {
		unblock()
		q.Stop()
	}()

	if _, err := q.Enqueue("Hung", []byte("payload"), "stuck"); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	if _, err := q.Enqueue("Fast", []byte("payload"), "receipt"); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	select {
	case printerName := <-fastDone:
		if printerName != "Fast" {
			t.Errorf("unexpected printer %q", printerName)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung printer blocked every other printer")
	}

	// Let the hung job finish so the test can shut down cleanly.
	unblock()
}

// TestBackoffFor checks the retry schedule is exponential and capped.
func TestBackoffFor(t *testing.T) {
	cases := map[int]time.Duration{
		0:  time.Second,
		1:  time.Second,
		2:  2 * time.Second,
		3:  4 * time.Second,
		4:  8 * time.Second,
		5:  MaxBackoff,
		20: MaxBackoff,
	}

	for attempt, want := range cases {
		if got := BackoffFor(attempt); got != want {
			t.Errorf("BackoffFor(%d) = %v, want %v", attempt, got, want)
		}
	}
}

// TestQueueRetryIsScheduledNotSlept proves a failed attempt is recorded with a future
// retry time instead of holding the worker loop.
func TestQueueRetryIsScheduledNotSlept(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	q, err := NewQueue(store,
		WithPrintFunc(func(string, []byte, string) error { return errors.New("printer offline") }),
		WithMaxAttempts(3),
	)
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	job, err := q.Enqueue("Offline", []byte("payload"), "job")
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// The first attempt happens immediately; the retry must then be parked.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stored, getErr := q.GetJobStatus(job.ID)
		if getErr != nil {
			t.Fatalf("GetJobStatus failed: %v", getErr)
		}
		if stored.Status == StatusRetrying {
			if stored.NextAttemptAt.IsZero() {
				t.Fatal("expected a scheduled retry time on a retrying job")
			}
			if !stored.NextAttemptAt.After(time.Now()) {
				t.Fatalf("expected the retry to be scheduled in the future, got %s", stored.NextAttemptAt)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}

	t.Fatal("job never entered the retrying state")
}

// TestQueuePauseBlocksDispatch proves pausing holds work rather than dropping it.
func TestQueuePauseBlocksDispatch(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	printed := make(chan string, 1)
	q, err := NewQueue(store, WithPrintFunc(func(printerName string, data []byte, jobName string) error {
		printed <- jobName
		return nil
	}))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	q.Pause()
	if !q.IsPaused() {
		t.Error("expected the queue to report itself paused")
	}

	if _, err := q.Enqueue("Printer", []byte("payload"), "held"); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	select {
	case jobName := <-printed:
		t.Fatalf("a paused queue dispatched %q", jobName)
	case <-time.After(400 * time.Millisecond):
	}

	q.Resume()
	select {
	case <-printed:
	case <-time.After(5 * time.Second):
		t.Fatal("resuming the queue did not dispatch the held job")
	}
}

// waitForStatus blocks until a job reaches the expected status.
func waitForStatus(t *testing.T, q *Queue, jobID string, want JobStatus) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err := q.GetJobStatus(jobID)
		if err != nil {
			t.Fatalf("GetJobStatus failed: %v", err)
		}
		if job.Status == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}

	job, _ := q.GetJobStatus(jobID)
	t.Fatalf("job %s never reached status %q (last seen: %q after %d attempts)",
		jobID, want, job.Status, job.Attempts)
}

// TestQueueDeadLetterAndRequeue covers the operator path: a job that exhausts its
// attempts lands in the dead-letter state and can be retried on demand.
func TestQueueDeadLetterAndRequeue(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	attempts := 0
	q, err := NewQueue(store, WithPrintFunc(func(string, []byte, string) error {
		mu.Lock()
		attempts++
		fail := attempts == 1
		mu.Unlock()

		if fail {
			return errors.New("printer offline")
		}
		return nil
	}), WithMaxAttempts(1))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	job, err := q.Enqueue("Printer", []byte("payload"), "job")
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	waitForStatus(t, q, job.ID, StatusFailed)

	// A dead-letter job is not retried on its own...
	time.Sleep(300 * time.Millisecond)
	stored, _ := q.GetJobStatus(job.ID)
	if stored.Attempts != 1 {
		t.Errorf("expected the dead-letter job to stay put, got %d attempts", stored.Attempts)
	}

	// ...but requeueing resets the attempt count and runs it again.
	if err := q.RequeueJob(job.ID); err != nil {
		t.Fatalf("RequeueJob failed: %v", err)
	}
	waitForStatus(t, q, job.ID, StatusSuccess)

	if err := q.RequeueJob("does-not-exist"); err == nil {
		t.Error("expected requeueing an unknown job to fail")
	}
}

// TestJobSummaryMatchesFullRead guards the two scanners against drifting apart: the
// dashboard read omits the payload but must agree on everything else.
func TestJobSummaryMatchesFullRead(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	original := &Job{
		ID:             "job-summary-check",
		PrinterName:    "Kitchen",
		JobName:        "Order #1",
		Payload:        []byte("payload"),
		Status:         StatusRetrying,
		CreatedAt:      time.Now().Truncate(time.Second),
		Attempts:       2,
		MaxAttempts:    5,
		ErrorMessage:   "paper jam",
		Priority:       7,
		IdempotencyKey: "order-1",
		NextAttemptAt:  time.Now().Add(time.Minute).Truncate(time.Second),
	}
	if err := store.CreateJob(original); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	full, err := store.GetJob(original.ID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}

	summaries, err := store.ListRecentJobs(10)
	if err != nil {
		t.Fatalf("ListRecentJobs failed: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected one summary row, got %d", len(summaries))
	}
	summary := summaries[0]

	if summary.ID != full.ID || summary.Status != full.Status ||
		summary.Priority != full.Priority || summary.Attempts != full.Attempts ||
		summary.MaxAttempts != full.MaxAttempts || summary.ErrorMessage != full.ErrorMessage ||
		summary.IdempotencyKey != full.IdempotencyKey {
		t.Errorf("summary and full read disagree:\n summary=%+v\n full=%+v", summary, full)
	}
	if !summary.NextAttemptAt.Equal(full.NextAttemptAt) {
		t.Errorf("next attempt differs: %s vs %s", summary.NextAttemptAt, full.NextAttemptAt)
	}
	if summary.Payload != nil {
		t.Error("the dashboard summary must not carry the payload")
	}
	if full.Priority != 7 || full.IdempotencyKey != "order-1" {
		t.Errorf("the new job columns did not round trip: %+v", full)
	}
	if !full.NextAttemptAt.Equal(original.NextAttemptAt) {
		t.Errorf("next_attempt_at did not round trip: %s vs %s", full.NextAttemptAt, original.NextAttemptAt)
	}
}