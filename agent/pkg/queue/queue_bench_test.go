package queue

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkQueueEnqueue(b *testing.B) {
	store, err := NewStore(":memory:")
	if err != nil {
		b.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	q, err := NewQueue(store, WithMaxAttempts(1))
	if err != nil {
		b.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	payload := make([]byte, 256)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := q.Enqueue(fmt.Sprintf("Printer_%d", i%10), payload, "Benchmark Job")
		if err != nil {
			b.Fatalf("enqueue failed: %v", err)
		}
	}
}

func BenchmarkQueueEnqueueAndProcess(b *testing.B) {
	store, err := NewStore(":memory:")
	if err != nil {
		b.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	var printCount int32
	mockPrint := func(printerName string, data []byte, jobName string) error {
		atomic.AddInt32(&printCount, 1)
		return nil
	}

	q, err := NewQueue(store, WithPrintFunc(mockPrint), WithMaxAttempts(1))
	if err != nil {
		b.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	payload := make([]byte, 256)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := q.Enqueue("BenchPrinter", payload, "Bench Job")
		if err != nil {
			b.Fatalf("enqueue failed: %v", err)
		}
	}

	// Wait for all jobs to be processed
	for atomic.LoadInt32(&printCount) < int32(b.N) {
		time.Sleep(10 * time.Millisecond)
	}
}

func BenchmarkStoreCreateJob(b *testing.B) {
	store, err := NewStore(":memory:")
	if err != nil {
		b.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	payload := make([]byte, 1024)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		job := &Job{
			ID:          fmt.Sprintf("bench-job-%d", i),
			PrinterName: "BenchPrinter",
			JobName:     "Bench Job",
			Payload:     payload,
			Status:      StatusQueued,
			CreatedAt:   time.Now(),
			Attempts:    0,
			MaxAttempts: 5,
		}
		if err := store.CreateJob(job); err != nil {
			b.Fatalf("CreateJob failed: %v", err)
		}
	}
}

func BenchmarkStoreGetJob(b *testing.B) {
	store, err := NewStore(":memory:")
	if err != nil {
		b.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	job := &Job{
		ID:          "bench-get-job",
		PrinterName: "BenchPrinter",
		JobName:     "Bench Job",
		Payload:     []byte("payload"),
		Status:      StatusQueued,
		CreatedAt:   time.Now(),
		Attempts:    0,
		MaxAttempts: 5,
	}
	store.CreateJob(job)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.GetJob("bench-get-job")
		if err != nil {
			b.Fatalf("GetJob failed: %v", err)
		}
	}
}
