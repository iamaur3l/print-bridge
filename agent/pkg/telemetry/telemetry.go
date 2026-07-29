package telemetry

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

type PrinterHealth struct {
	PrinterName       string    `json:"printer_name"`
	DriverName        string    `json:"driver_name"`
	PortName          string    `json:"port_name"`
	IsOnline          bool      `json:"is_online"`
	StatusDescription string    `json:"status_description"`
	LastSeenAt        time.Time `json:"last_seen_at"`
}

type Event struct {
	Type      string        `json:"type"`
	Printer   PrinterHealth `json:"printer"`
	Timestamp time.Time     `json:"timestamp"`
}

type Monitor struct {
	store       *queue.Store
	interval    time.Duration
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	mu          sync.RWMutex
	lastState   map[string]PrinterHealth
	subscribers map[chan Event]struct{}
	listFunc    func() ([]printer.PrinterInfo, error)
}

func NewMonitor(store *queue.Store, interval time.Duration) *Monitor {
	if interval <= 0 {
		interval = 30 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Monitor{
		store:       store,
		interval:    interval,
		ctx:         ctx,
		cancel:      cancel,
		lastState:   make(map[string]PrinterHealth),
		subscribers: make(map[chan Event]struct{}),
		listFunc:    printer.ListPrinters,
	}
}

// SetListFunc allows overriding printer discovery function during tests.
func (m *Monitor) SetListFunc(f func() ([]printer.PrinterInfo, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listFunc = f
}

// Start begins periodic printer health polling.
func (m *Monitor) Start() {
	m.wg.Add(1)
	go m.run()
}

// Stop gracefully terminates the telemetry monitor.
func (m *Monitor) Stop() {
	m.cancel()
	m.wg.Wait()
}

// Subscribe returns a channel receiving real-time printer status change events.
func (m *Monitor) Subscribe() chan Event {
	m.mu.Lock()
	defer m.mu.Unlock()

	ch := make(chan Event, 50)
	m.subscribers[ch] = struct{}{}
	return ch
}

// Unsubscribe removes a telemetry subscriber channel.
func (m *Monitor) Unsubscribe(ch chan Event) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.subscribers[ch]; exists {
		delete(m.subscribers, ch)
		close(ch)
	}
}

func (m *Monitor) run() {
	defer m.wg.Done()

	// Initial poll on startup
	m.PollOnce()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.PollOnce()
		}
	}
}

// PollOnce inspects current system printer states and broadcasts changes.
func (m *Monitor) PollOnce() {
	m.mu.RLock()
	listFn := m.listFunc
	m.mu.RUnlock()

	printers, err := listFn()
	if err != nil {
		log.Printf("[Telemetry] Error listing printers: %v", err)
		return
	}

	now := time.Now()
	freshMap := make(map[string]PrinterHealth)

	m.mu.Lock()
	for _, p := range printers {
		ph := PrinterHealth{
			PrinterName:       p.Name,
			DriverName:        p.DriverName,
			PortName:          p.PortName,
			IsOnline:          p.IsOnline,
			StatusDescription: p.StatusDescription,
			LastSeenAt:        now,
		}
		freshMap[p.Name] = ph

		prev, exists := m.lastState[p.Name]
		if !exists || prev.IsOnline != ph.IsOnline || prev.StatusDescription != ph.StatusDescription {
			log.Printf("[Telemetry] Printer %q status changed: %s -> %s (Online: %t)",
				p.Name, prev.StatusDescription, ph.StatusDescription, ph.IsOnline)

			event := Event{
				Type:      "printer_status_changed",
				Printer:   ph,
				Timestamp: now,
			}
			m.broadcastEvent(event)
		}

		// Persist health to SQLite
		m.saveTelemetry(ph)
	}
	m.lastState = freshMap
	m.mu.Unlock()
}

func (m *Monitor) broadcastEvent(event Event) {
	for ch := range m.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (m *Monitor) saveTelemetry(ph PrinterHealth) {
	if m.store == nil || m.store.DB() == nil {
		return
	}
	query := `
	INSERT INTO printer_telemetry (printer_name, driver_name, port_name, is_online, status_description, last_seen_at)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(printer_name) DO UPDATE SET
		driver_name = excluded.driver_name,
		port_name = excluded.port_name,
		is_online = excluded.is_online,
		status_description = excluded.status_description,
		last_seen_at = excluded.last_seen_at
	`
	isOnlineInt := 0
	if ph.IsOnline {
		isOnlineInt = 1
	}
	_, _ = m.store.DB().Exec(query,
		ph.PrinterName,
		ph.DriverName,
		ph.PortName,
		isOnlineInt,
		ph.StatusDescription,
		ph.LastSeenAt.Format(time.RFC3339),
	)
}

// GetHealth returns current printer telemetry for all discovered devices.
func (m *Monitor) GetHealth() ([]PrinterHealth, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.store == nil || m.store.DB() == nil {
		result := make([]PrinterHealth, 0, len(m.lastState))
		for _, ph := range m.lastState {
			result = append(result, ph)
		}
		return result, nil
	}

	query := `SELECT printer_name, driver_name, port_name, is_online, status_description, last_seen_at FROM printer_telemetry`
	rows, err := m.store.DB().Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PrinterHealth
	for rows.Next() {
		var ph PrinterHealth
		var isOnlineInt int
		var lastSeenStr sql.NullString

		if err := rows.Scan(&ph.PrinterName, &ph.DriverName, &ph.PortName, &isOnlineInt, &ph.StatusDescription, &lastSeenStr); err != nil {
			return nil, err
		}
		ph.IsOnline = isOnlineInt == 1
		if lastSeenStr.Valid {
			ph.LastSeenAt, _ = time.Parse(time.RFC3339, lastSeenStr.String)
		}
		result = append(result, ph)
	}
	return result, nil
}
