package queue

import (
	"context"
	"fmt"
	"log"
	"sync"
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
	mu          sync.RWMutex
	subscribers map[chan *Job]struct{}
	maxAttempts int
}

// Option configures Queue settings.
type Option func(*Queue)

func WithPrintFunc(f PrintFunc) Option {
	return func(q *Queue) {
		q.printFunc = f
	}
}

func WithMaxAttempts(attempts int) Option {
	return func(q *Queue) {
		q.maxAttempts = attempts
	}
}

// NewQueue creates and starts a background print job queue.
func NewQueue(store *Store, opts ...Option) (*Queue, error) {
	ctx, cancel := context.WithCancel(context.Background())

	q := &Queue{
		store:       store,
		printFunc:   printer.PrintRaw,
		notifyChan:  make(chan struct{}, 1),
		ctx:         ctx,
		cancel:      cancel,
		subscribers: make(map[chan *Job]struct{}),
		maxAttempts: 5,
	}

	for _, opt := range opts {
		opt(q)
	}

	// Startup recovery: reset stuck inflight jobs back to queued state
	if resetCount, err := store.ResetInflightJobs(); err == nil && resetCount > 0 {
		log.Printf("[Queue] Requeued %d jobs left in 'sending' state from previous run", resetCount)
	}

	q.wg.Add(1)
	go q.workerLoop()

	return q, nil
}

// Stop gracefully shuts down the queue worker loop.
func (q *Queue) Stop() {
	q.cancel()
	q.wg.Wait()
}

// Enqueue creates a new job and queues it for delivery.
func (q *Queue) Enqueue(printerName string, payload []byte, jobName string) (*Job, error) {
	if printerName == "" {
		return nil, fmt.Errorf("printerName is required")
	}
	if err := printer.VerifyPayload(payload); err != nil {
		return nil, err
	}

	jobID := uuid.New().String()
	job := &Job{
		ID:          jobID,
		PrinterName: printerName,
		JobName:     jobName,
		Payload:     payload,
		Status:      StatusQueued,
		CreatedAt:   time.Now(),
		Attempts:    0,
		MaxAttempts: q.maxAttempts,
	}

	if err := q.store.CreateJob(job); err != nil {
		return nil, fmt.Errorf("failed to enqueue job in store: %w", err)
	}

	q.notifyWorker()
	q.notifySubscribers(job)

	return job, nil
}

// GetJobStatus returns the current status of a job.
func (q *Queue) GetJobStatus(jobID string) (*Job, error) {
	return q.store.GetJob(jobID)
}

// ListRecentJobs returns recent jobs up to the specified limit.
func (q *Queue) ListRecentJobs(limit int) ([]*Job, error) {
	return q.store.ListRecentJobs(limit)
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

func (q *Queue) workerLoop() {
	defer q.wg.Done()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-q.ctx.Done():
			return
		case <-q.notifyChan:
			q.processPendingJobs()
		case <-ticker.C:
			q.processPendingJobs()
		}
	}
}

func (q *Queue) processPendingJobs() {
	pending, err := q.store.GetPendingJobs()
	if err != nil || len(pending) == 0 {
		return
	}

	for _, job := range pending {
		select {
		case <-q.ctx.Done():
			return
		default:
		}

		q.processJob(job)
	}
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
		q.notifySubscribers(job)
		log.Printf("[Queue] Job %s printed successfully to %q (attempts: %d)", job.ID, job.PrinterName, job.Attempts)
		return
	}

	// Handle delivery failure
	log.Printf("[Queue] Job %s failed attempt %d/%d: %v", job.ID, job.Attempts, job.MaxAttempts, err)

	if job.Attempts >= job.MaxAttempts {
		_ = q.store.UpdateJobStatus(job.ID, StatusFailed, job.Attempts, err.Error())
		job.Status = StatusFailed
		job.ErrorMessage = err.Error()
		q.notifySubscribers(job)
		log.Printf("[Queue] Job %s permanently marked FAILED after %d attempts", job.ID, job.Attempts)
		return
	}

	// Schedule for retry
	_ = q.store.UpdateJobStatus(job.ID, StatusRetrying, job.Attempts, err.Error())
	job.Status = StatusRetrying
	job.ErrorMessage = err.Error()
	q.notifySubscribers(job)

	// Exponential backoff wait (capped at max 10 seconds for local queue responsiveness)
	backoff := time.Duration(1<<uint(job.Attempts-1)) * time.Second
	if backoff > 10*time.Second {
		backoff = 10 * time.Second
	}
	time.Sleep(backoff)
}
