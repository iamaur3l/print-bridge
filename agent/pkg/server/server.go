package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/printbridge/printbridge/agent/pkg/auth"
	"github.com/printbridge/printbridge/agent/pkg/dashboard"
	"github.com/printbridge/printbridge/agent/pkg/devices"
	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
	"github.com/printbridge/printbridge/agent/pkg/telemetry"
	"github.com/printbridge/printbridge/agent/pkg/updater"
)

type rateLimiter struct {
	mu       sync.Mutex
	requests map[string]int
	window   time.Duration
	maxReqs  int
}

func newRateLimiter(maxReqs int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		requests: make(map[string]int),
		window:   window,
		maxReqs:  maxReqs,
	}
}

func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.requests[key]++
	if rl.requests[key] > rl.maxReqs {
		return false
	}

	// Reset after window expires
	go func() {
		time.Sleep(rl.window)
		rl.mu.Lock()
		rl.requests[key]--
		if rl.requests[key] <= 0 {
			delete(rl.requests, key)
		}
		rl.mu.Unlock()
	}()

	return true
}

type WSRequest struct {
	Version int            `json:"version"`
	ID      string         `json:"id,omitempty"`
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

type WSResponse struct {
	Version int    `json:"version"`
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Success bool   `json:"success"`
	Payload any    `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
}

type Server struct {
	addr          string
	queue         *queue.Queue
	auth          *auth.Manager
	telemetry     *telemetry.Monitor
	dashboard     *dashboard.Dashboard
	updater       *updater.Updater
	httpServer    *http.Server
	upgrader      websocket.Upgrader
	listener      net.Listener
	mu            sync.Mutex
	clients       map[*clientConn]struct{}
	tlsCertFile   string
	tlsKeyFile    string
	rateLimiter   *rateLimiter
	pairLimiter   *rateLimiter
	clientIP      func(r *http.Request) string
}

type clientConn struct {
	ws            *websocket.Conn
	mu            sync.Mutex
	send          chan []byte
	authenticated bool
	origin        string
}

func (c *clientConn) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteJSON(v)
}

type ServerOption func(*Server)

func WithTLSCert(certFile, keyFile string) ServerOption {
	return func(s *Server) {
		s.tlsCertFile = certFile
		s.tlsKeyFile = keyFile
	}
}

func NewServer(addr string, q *queue.Queue, authMgr *auth.Manager, tel *telemetry.Monitor, dash *dashboard.Dashboard, up *updater.Updater, opts ...ServerOption) *Server {
	if addr == "" {
		addr = "localhost:9567"
	}

	s := &Server{
		addr:      addr,
		queue:     q,
		auth:      authMgr,
		telemetry: tel,
		dashboard: dash,
		updater:   up,
		clients:   make(map[*clientConn]struct{}),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		rateLimiter: newRateLimiter(60, 1*time.Minute),
		pairLimiter: newRateLimiter(10, 1*time.Minute),
		clientIP: func(r *http.Request) string {
			if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
				return fwd
			}
			if r.RemoteAddr != "" {
				if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
					return host
				}
				return r.RemoteAddr
			}
			return "unknown"
		},
	}

	for _, opt := range opts {
		opt(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)

	if dash != nil {
		mux.Handle("/dashboard", dash)
		mux.Handle("/dashboard/", dash)
		mux.Handle("/api/dashboard/", dash)
	}

	// Fallback root WebSocket connection
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && r.Header.Get("Upgrade") == "websocket" {
			s.handleWS(w, r)
		} else if dash != nil {
			dash.ServeHTTP(w, r)
		} else {
			s.handleWS(w, r)
		}
	})

	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	return s
}

// Addr returns the listening network address.
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.addr
}

// Start begins serving WebSocket & HTTP Dashboard.
// If TLS cert/key files are configured, serves WSS/HTTPS.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}
	s.listener = ln

	scheme := "http"
	if s.tlsCertFile != "" && s.tlsKeyFile != "" {
		scheme = "https"
		log.Printf("[Server] TLS configured with cert=%s key=%s", s.tlsCertFile, s.tlsKeyFile)
	}

	log.Printf("[Server] WebSocket & Dashboard server listening on %s://%s", scheme, ln.Addr().String())

	go func() {
		var serveErr error
		if s.tlsCertFile != "" && s.tlsKeyFile != "" {
			serveErr = s.httpServer.ServeTLS(ln, s.tlsCertFile, s.tlsKeyFile)
		} else {
			serveErr = s.httpServer.Serve(ln)
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("[Server] HTTP server error: %v", serveErr)
		}
	}()

	return nil
}

// Stop gracefully shuts down the WebSocket server.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s.mu.Lock()
	for client := range s.clients {
		client.ws.Close()
	}
	s.mu.Unlock()

	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "http://localhost"
	}

	ip := s.clientIP(r)
	if !s.rateLimiter.Allow(ip) {
		log.Printf("[Server] Rate limit exceeded for IP %s", ip)
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[Server] WebSocket upgrade error: %v", err)
		return
	}

	client := &clientConn{
		ws:            ws,
		send:          make(chan []byte, 64),
		authenticated: false,
		origin:        origin,
	}

	s.mu.Lock()
	s.clients[client] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.clients, client)
		s.mu.Unlock()
		ws.Close()
	}()

	// Subscribe client to real-time job updates from the queue
	jobSub := s.queue.Subscribe()
	defer s.queue.Unsubscribe(jobSub)

	go func() {
		for job := range jobSub {
			updateMsg := WSResponse{
				Version: 1,
				Type:    "job_status_changed",
				Success: true,
				Payload: job,
			}
			_ = client.writeJSON(updateMsg)
		}
	}()

	// Subscribe client to telemetry printer health changes
	if s.telemetry != nil {
		telSub := s.telemetry.Subscribe()
		defer s.telemetry.Unsubscribe(telSub)

		go func() {
			for telEvent := range telSub {
				updateMsg := WSResponse{
					Version: 1,
					Type:    "printer_status_changed",
					Success: true,
					Payload: telEvent,
				}
				_ = client.writeJSON(updateMsg)
			}
		}()
	}

	for {
		var req WSRequest
		err := ws.ReadJSON(&req)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[Server] WebSocket read error: %v", err)
			}
			break
		}

		resp := s.dispatch(client, req)
		if err := client.writeJSON(resp); err != nil {
			log.Printf("[Server] WebSocket write error: %v", err)
			break
		}
	}
}

func (s *Server) dispatch(client *clientConn, req WSRequest) WSResponse {
	if req.Version != 1 {
		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: false,
			Error:   fmt.Sprintf("unsupported protocol version: %d (expected 1)", req.Version),
		}
	}

	switch req.Type {
	case "ping":
		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{"status": "pong", "time": time.Now().Format(time.RFC3339)},
		}

	case "request_pairing":
		if !s.pairLimiter.Allow(client.origin) {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: "too many pairing requests; try again later",
			}
		}
		appName, _ := req.Payload["app_name"].(string)
		if appName == "" {
			appName = "Web Client App"
		}
		code, expiresAt, err := s.auth.RequestPairing(appName, client.origin)
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   err.Error(),
			}
		}

		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{
				"code_requested": true,
				"code":           code,
				"expires_at":     expiresAt.Format(time.RFC3339),
			},
		}

	case "confirm_pairing":
		code, _ := req.Payload["code"].(string)
		if code == "" {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   "payload 'code' is required",
			}
		}

		pairedApp, err := s.auth.ConfirmPairing(code, client.origin)
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   err.Error(),
			}
		}

		client.authenticated = true

		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{
				"authenticated": true,
				"token":         pairedApp.Token,
				"app_id":        pairedApp.ID,
			},
		}

	case "authenticate":
		token, _ := req.Payload["token"].(string)
		if token == "" {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   "payload 'token' is required",
			}
		}

		valid, app := s.auth.ValidateToken(token, client.origin)
		if !valid {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   "invalid or revoked authentication token",
			}
		}

		client.authenticated = true

		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{
				"authenticated": true,
				"app_name":      app.AppName,
			},
		}

	case "list_printers":
		printers, err := printer.ListPrinters()
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   fmt.Sprintf("failed to list printers: %v", err),
			}
		}
		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{"printers": printers},
		}

	case "get_printer_health":
		if s.telemetry == nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   "telemetry monitor uninitialized",
			}
		}
		health, err := s.telemetry.GetHealth()
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   fmt.Sprintf("failed to get printer health: %v", err),
			}
		}
		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{"printers": health},
		}

	case "open_drawer":
		printerName, _ := req.Payload["printer"].(string)
		if printerName == "" {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   "payload 'printer' is required",
			}
		}

		pinNum := 2
		if pinVal, ok := req.Payload["pin"].(float64); ok {
			pinNum = int(pinVal)
		}
		brand, _ := req.Payload["brand"].(string)

		err := devices.OpenDrawer(printerName, devices.DrawerPin(pinNum), brand)
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   fmt.Sprintf("failed to kick cash drawer: %v", err),
			}
		}

		_ = s.queue.LogAuditEvent(queue.AuditDrawerKick, client.origin, "", fmt.Sprintf("drawer kick on %s (pin %d)", printerName, pinNum), "")

		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{"status": "drawer_kicked", "printer": printerName},
		}

	case "list_serial_ports":
		ports, err := devices.ListSerialPorts()
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   fmt.Sprintf("failed to list serial ports: %v", err),
			}
		}
		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{"ports": ports},
		}

	case "list_hid_devices":
		devs, err := devices.ListHIDDevices()
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   fmt.Sprintf("failed to list HID devices: %v", err),
			}
		}
		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{"devices": devs},
		}

	case "print":
		if s.auth != nil && !client.authenticated {
			token, _ := req.Payload["token"].(string)
			if token != "" {
				if valid, _ := s.auth.ValidateToken(token, client.origin); valid {
					client.authenticated = true
				}
			}
		}

		if s.auth != nil && !client.authenticated {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   "pairing_required",
			}
		}

		printerName, _ := req.Payload["printer"].(string)
		dataBase64, _ := req.Payload["data"].(string)
		jobName, _ := req.Payload["job_name"].(string)

		if printerName == "" {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   "payload 'printer' is required",
			}
		}

		if dataBase64 == "" {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   "payload 'data' (base64) is required",
			}
		}

		rawBytes, err := base64.StdEncoding.DecodeString(dataBase64)
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   fmt.Sprintf("invalid base64 payload: %v", err),
			}
		}

		job, err := s.queue.Enqueue(printerName, rawBytes, jobName)
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   fmt.Sprintf("failed to enqueue job: %v", err),
			}
		}

		_ = s.queue.LogAuditEvent(queue.AuditPrintJob, client.origin, "", fmt.Sprintf("print job %s on %s (size: %d bytes)", job.ID, printerName, len(rawBytes)), "")

		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{"job_id": job.ID, "status": job.Status},
		}

	case "job_status":
		jobID, _ := req.Payload["job_id"].(string)
		if jobID == "" {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   "payload 'job_id' is required",
			}
		}

		job, err := s.queue.GetJobStatus(jobID)
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   fmt.Sprintf("job not found: %v", err),
			}
		}

		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: job,
		}

	case "check_update":
		if s.updater == nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: "updater not initialized",
			}
		}
		vers, avail, err := s.updater.CheckForUpdate()
		if err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: fmt.Sprintf("update check failed: %v", err),
			}
		}
		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{
				"current_version": updater.Version,
				"available":       avail,
				"available_version": vers,
				"last_checked":    s.updater.LastCheckTime().Format(time.RFC3339),
			},
		}

	case "apply_update":
		if s.updater == nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: "updater not initialized",
			}
		}
		vers, avail, err := s.updater.CheckForUpdate()
		if err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: fmt.Sprintf("update check failed: %v", err),
			}
		}
		if !avail {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: "no update available",
			}
		}
		go func() {
			if err := s.updater.ApplyAvailableUpdate(); err != nil {
				log.Printf("[Server] Auto-update failed: %v", err)
			}
		}()
		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{
				"message":   fmt.Sprintf("downloading and applying version %s", vers),
				"version":   vers,
			},
		}

	default:
		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: false,
			Error:   fmt.Sprintf("unknown message type: %q", req.Type),
		}
	}
}
