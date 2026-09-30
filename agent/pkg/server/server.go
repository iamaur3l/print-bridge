package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/printbridge/printbridge/agent/pkg/auth"
	"github.com/printbridge/printbridge/agent/pkg/dashboard"
	"github.com/printbridge/printbridge/agent/pkg/devices"
	"github.com/printbridge/printbridge/agent/pkg/health"
	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
	"github.com/printbridge/printbridge/agent/pkg/roles"
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
	tlsCertFile    string
	tlsKeyFile     string
	rateLimiter    *rateLimiter
	pairLimiter    *rateLimiter
	clientIP       func(r *http.Request) string
	allowedOrigins []string
	trustedProxy   bool
	version        string
	startedAt      time.Time
	roles          *roles.Manager
	listPrinters   func() ([]printer.PrinterInfo, error)
}

type clientConn struct {
	ws            *websocket.Conn
	mu            sync.Mutex
	send          chan []byte
	authenticated atomic.Bool
	origin        string
	ip            string
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

// WithAllowedOrigins sets the browser origins permitted to open a WebSocket.
// Entries are exact origins ("https://pos.example.com"), host patterns with any
// port ("http://pos.local:*") or the wildcard "*". Loopback origins are always
// allowed in addition to whatever is listed here.
func WithAllowedOrigins(origins ...string) ServerOption {
	return func(s *Server) {
		for _, o := range origins {
			o = strings.TrimSpace(o)
			if o != "" {
				s.allowedOrigins = append(s.allowedOrigins, o)
			}
		}
	}
}

// WithTrustedProxy enables trusting X-Forwarded-For for client IP attribution.
// It is off by default because the agent binds loopback only and any local
// process can forge that header.
func WithTrustedProxy(trust bool) ServerOption {
	return func(s *Server) {
		s.trustedProxy = trust
	}
}

// WithAgentVersion records the version reported by GET /health.
func WithAgentVersion(version string) ServerOption {
	return func(s *Server) {
		s.version = version
	}
}

// WithRoleManager enables printer roles: addressing a station by name
// ("kitchen") instead of a device, plus the role management requests.
func WithRoleManager(manager *roles.Manager) ServerOption {
	return func(s *Server) {
		s.roles = manager
	}
}

// WithPrinterLister overrides printer discovery for this server. The default is the
// agent-wide cached snapshot; tests use it to avoid depending on the host's queues.
func WithPrinterLister(list func() ([]printer.PrinterInfo, error)) ServerOption {
	return func(s *Server) {
		if list != nil {
			s.listPrinters = list
		}
	}
}

// defaultLoopbackOrigins are trusted out of the box: the agent only listens on
// loopback, so a page served by this machine may reach it on any local port.
var defaultLoopbackOrigins = []string{
	"http://localhost:*",
	"http://127.0.0.1:*",
	"https://localhost:*",
	"https://127.0.0.1:*",
	"http://[::1]:*",
	"https://[::1]:*",
}

// AllowedOrigins returns the configured origin allowlist (used for startup logging).
func (s *Server) AllowedOrigins() []string {
	return append([]string(nil), s.allowedOrigins...)
}

// originAllowed reports whether a browser Origin header may connect.
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		// Non-browser clients (CLI tools, the relay client, integration tests)
		// do not send an Origin header.
		return true
	}
	for _, pattern := range s.allowedOrigins {
		if matchOrigin(pattern, origin) {
			return true
		}
	}
	return false
}

// matchOrigin matches exact origins, "scheme://host:*" (any port) and "*".
func matchOrigin(pattern, origin string) bool {
	if pattern == "" || origin == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ":*") {
		prefix := strings.ToLower(strings.TrimSuffix(pattern, "*"))
		return strings.HasPrefix(strings.ToLower(origin), prefix)
	}
	return strings.EqualFold(pattern, origin)
}

func NewServer(addr string, q *queue.Queue, authMgr *auth.Manager, tel *telemetry.Monitor, dash *dashboard.Dashboard, up *updater.Updater, opts ...ServerOption) *Server {
	if addr == "" {
		addr = "localhost:9567"
	}

	s := &Server{
		addr:        addr,
		queue:       q,
		auth:        authMgr,
		telemetry:   tel,
		dashboard:   dash,
		updater:     up,
		clients:     make(map[*clientConn]struct{}),
		rateLimiter: newRateLimiter(60, 1*time.Minute),
		pairLimiter: newRateLimiter(10, 1*time.Minute),
		startedAt:   time.Now(),
	}

	for _, opt := range opts {
		opt(s)
	}

	if s.listPrinters == nil {
		s.listPrinters = printer.ListPrintersCached
	}

	if len(s.allowedOrigins) > 0 {
		// Loopback origins are always trusted (the agent only listens on loopback);
		// anything else must be configured explicitly with -allow-origin.
		s.allowedOrigins = append(append([]string(nil), defaultLoopbackOrigins...), s.allowedOrigins...)
	} else {
		s.allowedOrigins = append([]string(nil), defaultLoopbackOrigins...)
	}

	s.clientIP = func(r *http.Request) string {
		if s.trustedProxy {
			if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
				if idx := strings.Index(fwd, ","); idx != -1 {
					fwd = fwd[:idx]
				}
				return strings.TrimSpace(fwd)
			}
		}
		if r.RemoteAddr != "" {
			if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
				return host
			}
			return r.RemoteAddr
		}
		return "unknown"
	}

	s.upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return s.originAllowed(r.Header.Get("Origin"))
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/health", s.handleHealth)

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

	// Best-effort port-binding bookkeeping: remember where each device-backed queue
	// lives so that a later cable move is detectable.
	s.refreshPortBindings()

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

// handleHealth serves the aggregate health verdict.
//
// It is readable without pairing (an installer checklist and a monitoring probe
// both need that) but it is not CORS-enabled, so a browser page cannot read it
// cross-origin, and the Host header must name this machine, which blocks
// DNS-rebinding attempts. It is also deliberately not a liveness check: an agent
// that is running but cannot print answers "degraded", not "healthy".
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !hostIsLocal(r.Host) {
		http.Error(w, fmt.Sprintf("host %q is not allowed for /health", r.Host), http.StatusForbidden)
		return
	}

	printers, printerErr := s.listPrinters()

	counts, err := s.queue.CountJobsByStatus()
	if err != nil {
		log.Printf("[Server] Failed to count jobs for /health: %v", err)
	}

	report := health.Build(health.Input{
		Version:     s.version,
		Port:        s.port(),
		StartedAt:   s.startedAt,
		Printers:    printers,
		Queue:       counts,
		SnapshotAge: printer.PrinterSnapshotAge(),
		PrinterErr:  printerErr,
	})

	w.Header().Set("Content-Type", "application/json")
	if report.Status == health.Unhealthy {
		// Convention: an agent that cannot do its job at all answers 503.
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(report)
}

// hostIsLocal reports whether a Host header names this machine.
func hostIsLocal(host string) bool {
	if host == "" {
		return true // non-browser client that omitted Host
	}

	h := host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		h = parsed
	}
	h = strings.Trim(strings.ToLower(h), "[]")

	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// port reports the numeric port the agent is listening on.
func (s *Server) port() int {
	_, portStr, err := net.SplitHostPort(s.Addr())
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return p
}

// diagnosePorts compares recorded bindings with the live device map.
func (s *Server) diagnosePorts(live []printer.LivePortMapping) []printer.Resolution {
	if live == nil {
		var err error
		live, err = printer.LivePortMappings()
		if err != nil {
			log.Printf("[Server] Failed to read live port mappings: %v", err)
			return []printer.Resolution{}
		}
	}

	bindings, err := s.queue.ListPrinterBindings()
	if err != nil {
		log.Printf("[Server] Failed to read recorded port bindings: %v", err)
		return []printer.Resolution{}
	}

	resolutions := printer.PlanPortBindings(bindings, live)
	if resolutions == nil {
		return []printer.Resolution{}
	}
	return resolutions
}

// refreshPortBindings records where each device-backed queue currently lives and
// logs a diagnosis if any queue can no longer reach its printer. It is best effort:
// a failure here must never stop the agent from serving.
func (s *Server) refreshPortBindings() {
	printers, err := s.listPrinters()
	if err != nil {
		log.Printf("[Server] Port binding check skipped: %v", err)
		return
	}

	live, err := printer.LivePortMappings()
	if err != nil {
		log.Printf("[Server] Port binding check skipped: %v", err)
		return
	}

	bindings := printer.ObserveBindings(printers, live)
	for _, b := range bindings {
		if err := s.queue.SavePrinterBinding(b); err != nil {
			log.Printf("[Server] Failed to record port binding for %q: %v", b.PrinterName, err)
		}
	}

	log.Printf("[Server] Port check: %d device-backed queue(s) tracked, %d live device mapping(s) on this machine",
		len(bindings), len(live))

	summary := printer.SummariseResolutions(s.diagnosePorts(live))
	if summary != "all printer ports are present" {
		log.Printf("[Server] Printer port check: %s", summary)
	}
}

// resolveTargetPrinter returns the physical queue name a request addresses. Requests
// may name a role ("kitchen") or a printer directly, which is what lets a POS keep
// addressing a station after the hardware behind it is replaced.
func (s *Server) resolveTargetPrinter(req WSRequest) (string, error) {
	if role, _ := req.Payload["role"].(string); strings.TrimSpace(role) != "" {
		if s.roles == nil {
			return "", errors.New("printer roles are not available on this agent")
		}

		resolved, err := s.roles.Resolve(role)
		if err != nil {
			return "", err
		}
		return resolved.Name, nil
	}

	if name, _ := req.Payload["printer"].(string); strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name), nil
	}

	return "", errors.New("payload 'printer' or 'role' is required")
}

// payloadString reads an optional string field.
func payloadString(req WSRequest, key string) string {
	value, _ := req.Payload[key].(string)
	return strings.TrimSpace(value)
}

// payloadInt reads an optional integer field (JSON numbers decode as float64).
func payloadInt(req WSRequest, key string) int {
	switch value := req.Payload[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

// capabilitiesFromPayload decodes an optional capability profile from a request.
func capabilitiesFromPayload(raw any) (printer.Capabilities, error) {
	if raw == nil {
		return printer.DefaultCapabilities(), nil
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		return printer.Capabilities{}, fmt.Errorf("invalid capabilities: %w", err)
	}

	var capabilities printer.Capabilities
	if err := json.Unmarshal(encoded, &capabilities); err != nil {
		return printer.Capabilities{}, fmt.Errorf("invalid capabilities: %w", err)
	}

	return capabilities.WithDefaults(), nil
}

// capabilitiesForTarget prefers the profile of the role being addressed, then any
// role pointing at the printer, and falls back to a plain 80mm receipt profile.
func (s *Server) capabilitiesForTarget(req WSRequest, printerName string) printer.Capabilities {
	if s.roles == nil {
		return printer.DefaultCapabilities()
	}

	if role := payloadString(req, "role"); role != "" {
		if assigned, err := s.roles.Get(role); err == nil {
			return assigned.Capabilities
		}
	}

	if assignedRoles, err := s.roles.List(); err == nil {
		for _, assigned := range assignedRoles {
			if strings.EqualFold(assigned.PrinterName, printerName) {
				return assigned.Capabilities
			}
		}
	}

	return printer.DefaultCapabilities()
}

// findPrinter looks a printer up in the cached snapshot by name.
func (s *Server) findPrinter(name string) (printer.PrinterInfo, bool) {
	printers, err := s.listPrinters()
	if err != nil {
		return printer.PrinterInfo{}, false
	}

	for _, p := range printers {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return printer.PrinterInfo{}, false
}

// diagnosePrinter assembles a plain-language diagnosis for one queue: transport,
// port binding, reachability and role membership, with a `checks` list written to be
// read by whoever is standing at the till.
func (s *Server) diagnosePrinter(name string) map[string]any {
	checks := make([]string, 0, 6)
	diagnosis := map[string]any{"printer": name}

	info, installed := s.findPrinter(name)
	if !installed {
		diagnosis["installed"] = false
		for _, resolution := range s.diagnosePorts(nil) {
			if strings.EqualFold(resolution.PrinterName, name) {
				diagnosis["port_binding"] = resolution
				checks = append(checks, resolution.Reason)
			}
		}
		checks = append(checks, fmt.Sprintf("printer %q is not installed on this machine", name))
		diagnosis["checks"] = checks
		return diagnosis
	}

	diagnosis["installed"] = true
	diagnosis["type"] = info.Type
	diagnosis["port_name"] = info.PortName
	diagnosis["state"] = info.State
	diagnosis["status_description"] = info.StatusDescription
	diagnosis["status_detail"] = info.StatusDetail
	diagnosis["is_default"] = info.IsDefault
	diagnosis["stale_work_offline"] = info.StaleWorkOffline

	if info.NetworkAddress != "" {
		diagnosis["network_address"] = info.NetworkAddress
		checks = append(checks, fmt.Sprintf("transport: direct TCP to %s (the spooler is bypassed)", info.NetworkAddress))
	} else {
		checks = append(checks, fmt.Sprintf("transport: OS print queue on port %s", info.PortName))
	}

	if last := printer.LastSuccessfulWrite(name); !last.IsZero() {
		diagnosis["last_successful_write"] = last.UTC().Format(time.RFC3339)
		checks = append(checks, fmt.Sprintf("last successful raw print: %s ago", time.Since(last).Round(time.Second)))
	} else {
		checks = append(checks, "no successful raw print has been observed since the agent started")
	}

	if s.roles != nil {
		if roleNames, err := s.roles.RolesForPrinter(name); err == nil && len(roleNames) > 0 {
			diagnosis["roles"] = roleNames
			checks = append(checks, fmt.Sprintf("serves role(s): %s", strings.Join(roleNames, ", ")))
		} else {
			checks = append(checks, "no printer role points at this queue")
		}
	}

	for _, resolution := range s.diagnosePorts(nil) {
		if strings.EqualFold(resolution.PrinterName, name) {
			diagnosis["port_binding"] = resolution
			if resolution.NeedsAttention() {
				checks = append(checks, resolution.Reason)
			}
		}
	}

	switch info.State {
	case string(printer.StateUnknown):
		checks = append(checks, "spooler reports the queue offline, but nothing corroborates it - a successful print clears this")
	case string(printer.StateOffline):
		checks = append(checks, fmt.Sprintf("printer is offline: %s", info.StatusDescription))
	default:
		checks = append(checks, fmt.Sprintf("printer is %s: %s", info.State, info.StatusDescription))
	}

	diagnosis["checks"] = checks
	return diagnosis
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")

	// A browser always sends Origin, and WebSocket connections are not protected
	// by the same-origin policy: any page the user visits can reach ws://localhost.
	// Anything that is not explicitly allowed (or a non-browser client with no
	// Origin header at all) is refused before the upgrade.
	if !s.originAllowed(origin) {
		log.Printf("[Server] Rejected WebSocket connection from disallowed origin %q", origin)
		http.Error(w, fmt.Sprintf("origin %q is not allowed; restart the agent with -allow-origin %s", origin, origin), http.StatusForbidden)
		return
	}

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
		ws:     ws,
		send:   make(chan []byte, 64),
		origin: origin,
		ip:     ip,
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
			// Job payloads and printer names are only pushed to paired clients.
			if !client.authenticated.Load() {
				continue
			}
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
				if !client.authenticated.Load() {
					continue
				}
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

// preAuthMessages are the only message types a connection may send before it has
// paired or authenticated. Everything else requires a valid token, so an
// unpaired page cannot enumerate printers, kick a cash drawer, or trigger an
// agent update.
var preAuthMessages = map[string]bool{
	"ping":            true,
	"request_pairing": true,
	"confirm_pairing": true,
	"authenticate":    true,
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

	if s.auth != nil && !client.authenticated.Load() && !preAuthMessages[req.Type] {
		// Clients may present a token inline on the message that needs it.
		if token, _ := req.Payload["token"].(string); token != "" {
			if valid, _ := s.auth.ValidateToken(token, client.origin); valid {
				client.authenticated.Store(true)
			}
		}
	}

	if s.auth != nil && !client.authenticated.Load() && !preAuthMessages[req.Type] {
		_ = s.queue.LogAuditEvent(queue.AuditTokenRejected, client.origin, "",
			fmt.Sprintf("unauthenticated %q request rejected", req.Type), client.ip)
		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: false,
			Error:   "pairing_required",
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
		// The 6-digit code is deliberately NOT returned over the socket: it is the
		// out-of-band proof that a human with access to the machine approved this
		// pairing. Returning it here would let any page pair itself silently.
		// It is displayed in the tray (auth.ActivePairingCode) and the dashboard
		// (/api/dashboard/summary -> pairing_code) instead.
		_, expiresAt, err := s.auth.RequestPairing(appName, client.origin)
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

		client.authenticated.Store(true)

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

		client.authenticated.Store(true)

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
		printers, err := s.listPrinters()
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

	case "diagnose_printers":
		resolutions := s.diagnosePorts(nil)
		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{
				"summary":     printer.SummariseResolutions(resolutions),
				"resolutions": resolutions,
			},
		}

	case "list_printer_roles":
		if s.roles == nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: "printer roles are not available on this agent",
			}
		}

		statuses, err := s.roles.Statuses()
		if err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: fmt.Sprintf("failed to list printer roles: %v", err),
			}
		}

		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{"roles": statuses},
		}

	case "assign_printer_role":
		if s.roles == nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: "printer roles are not available on this agent",
			}
		}

		roleName, _ := req.Payload["role"].(string)
		printerName, _ := req.Payload["printer"].(string)
		label, _ := req.Payload["label"].(string)

		capabilities, err := capabilitiesFromPayload(req.Payload["capabilities"])
		if err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: err.Error(),
			}
		}

		assigned, err := s.roles.Assign(roleName, printerName, label, capabilities)
		if err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: err.Error(),
			}
		}

		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{
				"role":         assigned.Role,
				"printer":      assigned.PrinterName,
				"label":        assigned.Label,
				"capabilities": assigned.Capabilities,
			},
		}

	case "remove_printer_role":
		if s.roles == nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: "printer roles are not available on this agent",
			}
		}

		roleName, _ := req.Payload["role"].(string)
		if err := s.roles.Remove(roleName); err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: err.Error(),
			}
		}

		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{"role": roles.NormaliseRole(roleName)},
		}

	case "test_print":
		target, err := s.resolveTargetPrinter(req)
		if err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: err.Error(),
			}
		}

		roleName, _ := req.Payload["role"].(string)
		slip := printer.ParseTestSlip(payloadString(req, "slip"))
		caps := s.capabilitiesForTarget(req, target)
		payloadBytes := printer.BuildTestSlip(slip, roles.NormaliseRole(roleName), target, caps)

		job, err := s.queue.Enqueue(target, payloadBytes, fmt.Sprintf("PrintBridge Test Print (%s)", slip))
		if err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: fmt.Sprintf("failed to enqueue test print: %v", err),
			}
		}

		_ = s.queue.LogAuditEvent(queue.AuditPrintJob, client.origin, "",
			fmt.Sprintf("test print (%s) queued for %q", slip, target), client.ip)

		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{
				"job_id":  job.ID,
				"printer": target,
				"slip":    slip,
				"bytes":   len(payloadBytes),
			},
		}

	case "diagnose_printer":
		target, err := s.resolveTargetPrinter(req)
		if err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: err.Error(),
			}
		}

		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: s.diagnosePrinter(target),
		}

	case "queue_status":
		counts, err := s.queue.CountJobsByStatus()
		if err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: fmt.Sprintf("failed to read queue status: %v", err),
			}
		}

		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{
				"paused": s.queue.IsPaused(),
				"counts": counts,
			},
		}

	case "pause_queue":
		s.queue.Pause()
		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{"paused": true},
		}

	case "resume_queue":
		s.queue.Resume()
		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{"paused": false},
		}

	case "retry_job":
		jobID := payloadString(req, "job_id")
		if jobID == "" {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: "payload 'job_id' is required",
			}
		}

		if err := s.queue.RequeueJob(jobID); err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: fmt.Sprintf("failed to requeue job: %v", err),
			}
		}

		_ = s.queue.LogAuditEvent(queue.AuditPrintJob, client.origin, "",
			fmt.Sprintf("dead-letter job %s requeued", jobID), client.ip)

		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{"job_id": jobID, "status": string(queue.StatusQueued)},
		}

	case "clear_failed_jobs":
		removed, err := s.queue.ClearFailedJobs()
		if err != nil {
			return WSResponse{
				Version: 1, ID: req.ID, Type: "response", Success: false,
				Error: fmt.Sprintf("failed to clear failed jobs: %v", err),
			}
		}

		return WSResponse{
			Version: 1, ID: req.ID, Type: "response", Success: true,
			Payload: map[string]any{"removed": removed},
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
		printerName, err := s.resolveTargetPrinter(req)
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   err.Error(),
			}
		}

		pinNum := 2
		if pinVal, ok := req.Payload["pin"].(float64); ok {
			pinNum = int(pinVal)
		}
		brand, _ := req.Payload["brand"].(string)

		err = devices.OpenDrawer(printerName, devices.DrawerPin(pinNum), brand)
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   fmt.Sprintf("failed to kick cash drawer: %v", err),
			}
		}

		_ = s.queue.LogAuditEvent(queue.AuditDrawerKick, client.origin, "", fmt.Sprintf("drawer kick on %s (pin %d)", printerName, pinNum), client.ip)

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
		printerName, err := s.resolveTargetPrinter(req)
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   err.Error(),
			}
		}

		dataBase64, _ := req.Payload["data"].(string)
		jobName, _ := req.Payload["job_name"].(string)

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

		job, created, err := s.queue.EnqueueJob(printerName, rawBytes, queue.EnqueueOptions{
			JobName:        jobName,
			Priority:       payloadInt(req, "priority"),
			IdempotencyKey: payloadString(req, "idempotency_key"),
		})
		if err != nil {
			return WSResponse{
				Version: 1,
				ID:      req.ID,
				Type:    "response",
				Success: false,
				Error:   fmt.Sprintf("failed to enqueue job: %v", err),
			}
		}

		_ = s.queue.LogAuditEvent(queue.AuditPrintJob, client.origin, "", fmt.Sprintf("print job %s on %s (size: %d bytes)", job.ID, printerName, len(rawBytes)), client.ip)

		return WSResponse{
			Version: 1,
			ID:      req.ID,
			Type:    "response",
			Success: true,
			Payload: map[string]any{
				"job_id":    job.ID,
				"status":    job.Status,
				"created":   created,
				"duplicate": !created,
			},
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
