package printer

import (
	"sync"
	"time"
)

// DefaultSnapshotTTL is how long a printer enumeration is shared across the whole
// agent. One snapshot serves the health endpoint, telemetry, the dashboard and the
// list_printers RPC, so a health sweep across N printers costs one OS call rather
// than one per consumer.
const DefaultSnapshotTTL = 4 * time.Second

// Snapshotter caches printer enumerations with a TTL and collapses concurrent
// refreshes into a single call (single-flight), so a burst of callers at
// snapshot expiry still only hits the OS once.
type Snapshotter struct {
	ttl    time.Duration
	listFn func() ([]PrinterInfo, error)

	mu       sync.Mutex
	cached   []PrinterInfo
	cachedAt time.Time
	loaded   bool
	lastErr  error
	inflight chan struct{}
}

// NewSnapshotter builds a Snapshotter. A non-positive TTL falls back to
// DefaultSnapshotTTL; a nil list function falls back to ListPrinters.
func NewSnapshotter(ttl time.Duration, listFn func() ([]PrinterInfo, error)) *Snapshotter {
	if ttl <= 0 {
		ttl = DefaultSnapshotTTL
	}
	if listFn == nil {
		listFn = ListPrinters
	}

	return &Snapshotter{ttl: ttl, listFn: listFn}
}

// List returns the cached snapshot, refreshing it when the TTL has elapsed.
// Concurrent callers during a refresh share a single enumeration.
func (s *Snapshotter) List() ([]PrinterInfo, error) {
	s.mu.Lock()
	if s.loaded && time.Since(s.cachedAt) < s.ttl {
		out, err := copyPrinters(s.cached), s.lastErr
		s.mu.Unlock()
		return out, err
	}

	if s.inflight != nil {
		done := s.inflight
		s.mu.Unlock()
		<-done

		s.mu.Lock()
		out, err := copyPrinters(s.cached), s.lastErr
		s.mu.Unlock()
		return out, err
	}

	done := make(chan struct{})
	s.inflight = done
	s.mu.Unlock()

	list, err := s.listFn()

	s.mu.Lock()
	s.cached = list
	s.cachedAt = time.Now()
	s.loaded = true
	s.lastErr = err
	s.inflight = nil
	s.mu.Unlock()
	close(done)

	return copyPrinters(list), err
}

// Invalidate forces the next List to re-enumerate the OS.
func (s *Snapshotter) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = false
	s.cachedAt = time.Time{}
}

// Age reports how long ago the current snapshot was taken (0 when none exists).
func (s *Snapshotter) Age() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return 0
	}
	return time.Since(s.cachedAt)
}

func copyPrinters(in []PrinterInfo) []PrinterInfo {
	if in == nil {
		return nil
	}
	out := make([]PrinterInfo, len(in))
	copy(out, in)
	return out
}

var sharedSnapshot = NewSnapshotter(DefaultSnapshotTTL, ListPrinters)

// ListPrintersCached returns a shared, cached enumeration of installed printers.
// Use this instead of ListPrinters anywhere a request or poll might repeat.
func ListPrintersCached() ([]PrinterInfo, error) {
	return sharedSnapshot.List()
}

// InvalidatePrinterSnapshot forces the shared snapshot to refresh on next read.
func InvalidatePrinterSnapshot() {
	sharedSnapshot.Invalidate()
}

// PrinterSnapshotAge reports the age of the shared snapshot.
func PrinterSnapshotAge() time.Duration {
	return sharedSnapshot.Age()
}
