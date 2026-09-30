package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/printbridge/printbridge/agent/pkg/printer"
)

// PrintFunc type allows injecting print implementation (or mock during testing).
type PrintFunc func(printerName string, data []byte, jobName string) error

type Queue struct {
	store       *Store
	printFunc   PrintFunc
	notifyChan  chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	jobWG       sync.WaitGroup
	mu          sync.RWMutex
	subscribers map[chan *Job]struct{}
	maxAttempts int
	// maxDepth caps how many unfinished jobs may be queued, so a POS that has lost
	// its printer cannot exhaust memory or disk.
	maxDepth int
	// maxConcurrent bounds how many printers are worked on at once.
	maxConcurrent int
	paused        atomic.Bool
	inflightMu    sync.Mutex
	inflight      map[string]bool
}

// Defaults for queue limits.
const (
	DefaultMaxDepth          = 5000
	DefaultMaxConcurrentJobs = 4
)

// Option configures Queue settings.
type Option func(*Queue)

func WithPrintFunc(f PrintFunc) Option {
	return func(q *Queue) {
		q.printFunc = f
	}
}

func WithMaxAttempts(attempts int) Option {
	return func(q *Queue) {
		if attempts > 0 {
			q.maxAttempts = attempts
		}
	}
}

// WithMaxDepth caps the number of unfinished jobs the queue will hold.
func WithMaxDepth(depth int) Option {
	return func(q *Queue) {
		if depth > 0 {
			q.maxDepth = depth
		}
	}
}

// WithMaxConcurrentJobs bounds how many printers are served in parallel. Jobs for the
// same printer always run one at a time so a station keeps its order.
func WithMaxConcurrentJobs(limit int) Option {
	return func(q *Queue) {
		if limit > 0 {
			q.maxConcurrent = limit
		}
	}
}

// NewQueue creates and starts a background print job queue.
func NewQueue(store *Store, opts ...Option) (*Queue, error) {
	ctx, cancel := context.WithCancel(context.Background())

	q := &Queue{
		store:         store,
		printFunc:     printer.PrintRaw,
		notifyChan:    make(chan struct{}, 1),
		ctx:           ctx,
		cancel:        cancel,
		subscribers:   make(map[chan *Job]struct{}),
		maxAttempts:   5,
		maxDepth:      DefaultMaxDepth,
		maxConcurrent: DefaultMaxConcurrentJobs,
		inflight:      make(map[string]bool),
	}

	for _, opt := range opts {
		opt(q)
	}

	// Startup recovery: requeue jobs left mid-flight by a previous run, so an agent
	// restart never loses an accepted job.
	if resetCount, err := store.ResetInflightJobs(); err == nil && resetCount > 0 {
		log.Printf("[Queue] Requeued %d jobs left in 'sending' state from previous run", resetCount)
	}

	q.wg.Add(1)
	go q.workerLoop()

	return q, nil
}

// Stop gracefully shuts down the worker loop. Jobs already in flight are allowed to
// finish their current attempt before Stop returns.
func (q *Queue) Stop() {
	q.cancel()
	q.wg.Wait()
	q.jobWG.Wait()
}

// Pause stops dispatching without dropping work, which is what an operator wants
// while swapping hardware.
func (q *Queue) Pause() {
	q.paused.Store(true)
	log.Println("[Queue] Dispatch paused")
}

// Resume restarts dispatch.
func (q *Queue) Resume() {
	q.paused.Store(false)
	q.notifyWorker()
	log.Println("[Queue] Dispatch resumed")
}

// IsPaused reports whether dispatch is paused.
func (q *Queue) IsPaused() bool {
	return q.paused.Load()
}

// RequeueJob puts a dead-letter job back in the queue for another round of attempts.
func (q *Queue) RequeueJob(jobID string) error {
	if err := q.store.RequeueJob(jobID); err != nil {
		return err
	}
	q.notifyWorker()
	return nil
}

// ClearFailedJobs deletes dead-letter jobs.
func (q *Queue) ClearFailedJobs() (int64, error) {
	return q.store.DeleteFailedJobs()
}

// ErrQueueFull is returned when the queue already holds maxDepth unfinished jobs.
var ErrQueueFull = errors.New("print queue is full")

// EnqueueOptions carries the optional attributes of a job.
type EnqueueOptions struct {
	JobName string
	// Priority orders the queue; higher runs sooner.
	Priority int
	// IdempotencyKey makes enqueueing safe to retry: a repeat with the same key
	// returns the original job instead of queueing the receipt a second time.
	IdempotencyKey string
	// MaxAttempts overrides the queue default for this job.
	MaxAttempts int
}

// Enqueue creates a new job and queues it for delivery.
func (q *Queue) Enqueue(printerName string, payload []byte, jobName string) (*Job, error) {
	job, _, err := q.EnqueueJob(printerName, payload, EnqueueOptions{JobName: jobName})
	return job, err
}

// EnqueueJob creates a job with full options. The second return value is false when
// an existing job was reused because its idempotency key had already been seen.
func (q *Queue) EnqueueJob(printerName string, payload []byte, opts EnqueueOptions) (*Job, bool, error) {
	if strings.TrimSpace(printerName) == "" {
		return nil, false, fmt.Errorf("printerName is required")
	}
	if err := printer.VerifyPayload(payload); err != nil {
		return nil, false, err
	}

	key := strings.TrimSpace(opts.IdempotencyKey)

	// Idempotency first: a POS that retries its own request must not print twice.
	if key != "" {
		existing, err := q.store.FindJobByIdempotencyKey(key)
		switch {
		case err == nil:
			log.Printf("[Queue] Idempotent enqueue for key %q reused job %s", key, existing.ID)
			return existing, false, nil
		case !errors.Is(err, sql.ErrNoRows):
			return nil, false, fmt.Errorf("failed to check idempotency key: %w", err)
		}
	}

	// Depth limit: refuse rather than grow without bound while no printer works.
	unfinished, err := q.store.CountUnfinishedJobs()
	if err != nil {
		return nil, false, fmt.Errorf("failed to read queue depth: %w", err)
	}
	if q.maxDepth > 0 && unfinished >= q.maxDepth {
		return nil, false, fmt.Errorf("%w (%d unfinished jobs)", ErrQueueFull, unfinished)
	}

	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = q.maxAttempts
	}

	job := &Job{
		ID:             uuid.New().String(),
		PrinterName:    printerName,
		JobName:        opts.JobName,
		Payload:        payload,
		Status:         StatusQueued,
		CreatedAt:      time.Now(),
		Attempts:       0,
		MaxAttempts:    maxAttempts,
		Priority:       opts.Priority,
		IdempotencyKey: key,
	}

	if err := q.store.CreateJob(job); err != nil {
		// Two racing requests with the same key lose to the unique index; the winner's
		// job is the right answer for both.
		if key != "" && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			if existing, findErr := q.store.FindJobByIdempotencyKey(key); findErr == nil {
				return existing, false, nil
			}
		}
		return nil, false, fmt.Errorf("failed to enqueue job in store: %w", err)
	}

	q.notifyWorker()
	q.notifySubscribers(job)

	return job, true, nil
}

// GetJobStatus returns the current status of a job.
func (q *Queue) GetJobStatus(jobID string) (*Job, error) {
	return q.store.GetJob(jobID)
}

// ListRecentJobs returns recent jobs up to the specified limit.
func (q *Queue) ListRecentJobs(limit int) ([]*Job, error) {
	return q.store.ListRecentJobs(limit)
}

// CountJobsByStatus returns per-status job counts for health reporting.
func (q *Queue) CountJobsByStatus() (JobCounts, error) {
	return q.store.CountJobsByStatus()
}

// SavePrinterBinding records the last known good port for a queue.
func (q *Queue) SavePrinterBinding(b printer.PortBinding) error {
	return q.store.SavePrinterBinding(b)
}

// ListPrinterBindings returns the recorded port bindings.
func (q *Queue) ListPrinterBindings() ([]printer.PortBinding, error) {
	return q.store.ListPrinterBindings()
}

// LogAuditEvent records a security audit event in the SQLite store.
func (q *Queue) LogAuditEvent(eventType, origin, appName, details, ipAddr string) error {
	return q.store.LogAuditEvent(eventType, origin, appName, details, ipAddr)
}

// Subscribe returns a channel that receives live job update notifications.
func (q *Queue) Subscribe() chan *Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	ch := make(chan *Job, 50)
	q.subscribers[ch] = struct{}{}
	return ch
}

// Unsubscribe removes a job status update subscriber channel.
func (q *Queue) Unsubscribe(ch chan *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if _, exists := q.subscribers[ch]; exists {
		delete(q.subscribers, ch)
		close(ch)
	}
}

func (q *Queue) notifySubscribers(job *Job) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	for ch := range q.subscribers {
		select {
		case ch <- job:
		default:
			// Buffer full, skip to prevent blocking
		}
	}
}

func (q *Queue) notifyWorker() {
	select {
	case q.notifyChan <- struct{}{}:
	default:
	}
}

// MaxBackoff caps the retry delay so a local queue stays responsive.
const MaxBackoff = 10 * time.Second

// BackoffFor returns the exponential backoff before attempt n (1-based).
func BackoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	backoff := time.Duration(1<<uint(attempt-1)) * time.Second
	if backoff > MaxBackoff {
		backoff = MaxBackoff
	}
	return backoff
}

func (q *Queue) workerLoop() {
	defer q.wg.Done()

	// A one-second heartbeat plus the notify channel. Retries are scheduled in the
	// database (next_attempt_at), so this loop never sleeps on a single job and one
	// struggling printer cannot delay another station.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-q.ctx.Done():
			return
		case <-q.notifyChan:
			q.dispatchPending()
		case <-ticker.C:
			q.dispatchPending()
		}
	}
}

// dispatchPending starts one job per idle printer, up to the concurrency limit. Jobs
// for the same printer run strictly one at a time, which keeps per-station order
// even though printers are served in parallel.
func (q *Queue) dispatchPending() {
	if q.paused.Load() {
		return
	}

	pending, err := q.store.GetPendingJobs()
	if err != nil {
		log.Printf("[Queue] Failed to read pending jobs: %v", err)
		return
	}

	for _, job := range pending {
		select {
		case <-q.ctx.Done():
			return
		default:
		}

		if q.activePrinters() >= q.maxConcurrent {
			return
		}
		if !q.claimPrinter(job.PrinterName) {
			continue // that printer is already busy
		}

		q.jobWG.Add(1)
		go func(job *Job) {
			defer q.jobWG.Done()
			// Wake the dispatcher once this printer is free again, so a station's next
			// receipt starts immediately instead of waiting for the next tick.
			defer q.notifyWorker()
			defer q.releasePrinter(job.PrinterName)
			q.processJob(job)
		}(job)
	}
}

// claimPrinter reserves a printer for one job, reporting false when it is busy.
func (q *Queue) claimPrinter(printerName string) bool {
	q.inflightMu.Lock()
	defer q.inflightMu.Unlock()

	if q.inflight[printerName] {
		return false
	}
	q.inflight[printerName] = true
	return true
}

func (q *Queue) releasePrinter(printerName string) {
	q.inflightMu.Lock()
	delete(q.inflight, printerName)
	q.inflightMu.Unlock()
}

func (q *Queue) activePrinters() int {
	q.inflightMu.Lock()
	defer q.inflightMu.Unlock()
	return len(q.inflight)
}

func (q *Queue) processJob(job *Job) {
	// Mark sending
	_ = q.store.UpdateJobStatus(job.ID, StatusSending, job.Attempts, "")
	job.Status = StatusSending
	q.notifySubscribers(job)

	// Execute direct print delivery
	err := q.printFunc(job.PrinterName, job.Payload, job.JobName)
	job.Attempts++
	job.LastAttemptAt = time.Now()

	if err == nil {
		_ = q.store.UpdateJobStatus(job.ID, StatusSuccess, job.Attempts, "")
		job.Status = StatusSuccess
		job.ErrorMessage = ""
		job.NextAttemptAt = time.Time{}
		q.notifySubscribers(job)
		log.Printf("[Queue] Job %s printed successfully to %q (attempts: %d)", job.ID, job.PrinterName, job.Attempts)
		return
	}

	log.Printf("[Queue] Job %s failed attempt %d/%d on %q: %v",
		job.ID, job.Attempts, job.MaxAttempts, job.PrinterName, err)

	if job.Attempts >= job.MaxAttempts {
		_ = q.store.UpdateJobStatus(job.ID, StatusFailed, job.Attempts, err.Error())
		job.Status = StatusFailed
		job.ErrorMessage = err.Error()
		q.notifySubscribers(job)
		log.Printf("[Queue] Job %s dead-lettered after %d attempts", job.ID, job.Attempts)
		return
	}

	// Schedule the next attempt rather than sleeping here, so this printer being
	// stuck never blocks the others.
	nextAttempt := time.Now().Add(BackoffFor(job.Attempts))
	if err := q.store.ScheduleRetry(job.ID, job.Attempts, err.Error(), nextAttempt); err != nil {
		log.Printf("[Queue] Failed to schedule retry for job %s: %v", job.ID, err)
	}

	job.Status = StatusRetrying
	job.ErrorMessage = err.Error()
	job.NextAttemptAt = nextAttempt
	q.notifySubscribers(job)

	log.Printf("[Queue] Job %s retrying at %s (attempt %d/%d)",
		job.ID, nextAttempt.Format(time.RFC3339), job.Attempts, job.MaxAttempts)
}
