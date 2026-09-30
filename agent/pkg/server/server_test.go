package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/printbridge/printbridge/agent/pkg/auth"
	"github.com/printbridge/printbridge/agent/pkg/dashboard"
	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
	"github.com/printbridge/printbridge/agent/pkg/roles"
	"github.com/printbridge/printbridge/agent/pkg/telemetry"
	"github.com/printbridge/printbridge/agent/pkg/updater"
)

func TestWebSocketServerLifecycle(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	authMgr, err := auth.NewManager(store)
	if err != nil {
		t.Fatalf("failed to create auth manager: %v", err)
	}

	tel := telemetry.NewMonitor(store, 1*time.Minute)
	dash := dashboard.NewDashboard(store, authMgr)

	mockPrint := func(printerName string, data []byte, jobName string) error {
		return nil
	}

	q, err := queue.NewQueue(store, queue.WithPrintFunc(mockPrint))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	srv := NewServer("127.0.0.1:0", q, authMgr, tel, dash, &updater.Updater{})
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop()

	u := url.URL{Scheme: "ws", Host: srv.Addr(), Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("failed to dial websocket: %v", err)
	}
	defer conn.Close()

	// 1. Test ping
	pingReq := WSRequest{Version: 1, ID: "msg_ping", Type: "ping"}
	if err := conn.WriteJSON(pingReq); err != nil {
		t.Fatalf("write ping error: %v", err)
	}

	var pingResp WSResponse
	if err := conn.ReadJSON(&pingResp); err != nil {
		t.Fatalf("read ping response error: %v", err)
	}

	if !pingResp.Success || pingResp.ID != "msg_ping" {
		t.Errorf("unexpected ping response: %+v", pingResp)
	}

	// 2. Test unpaired print rejection (pairing_required)
	unpairedPrintReq := WSRequest{
		Version: 1,
		ID:      "msg_unpaired_print",
		Type:    "print",
		Payload: map[string]any{"printer": "MockPrinter", "data": "SGVsbG8="},
	}
	if err := conn.WriteJSON(unpairedPrintReq); err != nil {
		t.Fatalf("write unpaired print error: %v", err)
	}
	var unpairedResp WSResponse
	if err := conn.ReadJSON(&unpairedResp); err != nil {
		t.Fatalf("read unpaired response error: %v", err)
	}
	if unpairedResp.Success || unpairedResp.Error != "pairing_required" {
		t.Errorf("expected pairing_required error, got: %+v", unpairedResp)
	}

	// 3. Test pairing flow (request_pairing -> confirm_pairing)
	pairReq := WSRequest{
		Version: 1,
		ID:      "msg_req_pair",
		Type:    "request_pairing",
		Payload: map[string]any{"app_name": "Test POS App"},
	}
	if err := conn.WriteJSON(pairReq); err != nil {
		t.Fatalf("write request_pairing error: %v", err)
	}

	var pairResp WSResponse
	if err := conn.ReadJSON(&pairResp); err != nil {
		t.Fatalf("read request_pairing response error: %v", err)
	}

	payloadMap, _ := pairResp.Payload.(map[string]any)
	if _, leaked := payloadMap["code"]; leaked {
		t.Fatalf("the pairing code must never be returned over the WebSocket: %v", payloadMap)
	}

	// The human reads the code from the tray / dashboard, not from the socket.
	code, _, ok := authMgr.ActivePairingCode()
	if !ok {
		t.Fatal("expected an active pairing code for the tray / dashboard to display")
	}

	confirmReq := WSRequest{
		Version: 1,
		ID:      "msg_confirm_pair",
		Type:    "confirm_pairing",
		Payload: map[string]any{"code": code},
	}
	if err := conn.WriteJSON(confirmReq); err != nil {
		t.Fatalf("write confirm_pairing error: %v", err)
	}

	var confirmResp WSResponse
	if err := conn.ReadJSON(&confirmResp); err != nil {
		t.Fatalf("read confirm_pairing response error: %v", err)
	}

	if !confirmResp.Success {
		t.Fatalf("confirm_pairing failed: %s", confirmResp.Error)
	}

	confirmPayload, _ := confirmResp.Payload.(map[string]any)
	token := confirmPayload["token"].(string)

	// 4. Test get_printer_health
	healthReq := WSRequest{Version: 1, ID: "msg_health", Type: "get_printer_health"}
	if err := conn.WriteJSON(healthReq); err != nil {
		t.Fatalf("write get_printer_health error: %v", err)
	}
	var healthResp WSResponse
	if err := conn.ReadJSON(&healthResp); err != nil {
		t.Fatalf("read get_printer_health response error: %v", err)
	}
	if !healthResp.Success {
		t.Errorf("get_printer_health failed: %s", healthResp.Error)
	}

	// 5. Test authenticated print request
	printData := base64.StdEncoding.EncodeToString([]byte("Hello PrintBridge Dashboard Integration"))
	printReq := WSRequest{
		Version: 1,
		ID:      "msg_print",
		Type:    "print",
		Payload: map[string]any{
			"printer":  "MockPrinter",
			"data":     printData,
			"job_name": "Dashboard Test Job",
			"token":    token,
		},
	}

	if err := conn.WriteJSON(printReq); err != nil {
		t.Fatalf("write print request error: %v", err)
	}

	var printResp WSResponse
	if err := conn.ReadJSON(&printResp); err != nil {
		t.Fatalf("read print response error: %v", err)
	}

	if !printResp.Success {
		t.Fatalf("authenticated print request failed: %s", printResp.Error)
	}
}

// --- Security regression tests (P0) ---

// newTestServer starts a loopback agent with a mock printer backend and returns
// the server, its auth manager, and a counter of real print side effects.
func newTestServer(t *testing.T, opts ...ServerOption) (*Server, *auth.Manager, *int32) {
	t.Helper()

	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	authMgr, err := auth.NewManager(store)
	if err != nil {
		t.Fatalf("failed to create auth manager: %v", err)
	}

	var printCount int32
	mockPrint := func(printerName string, data []byte, jobName string) error {
		atomic.AddInt32(&printCount, 1)
		return nil
	}

	q, err := queue.NewQueue(store, queue.WithPrintFunc(mockPrint))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	t.Cleanup(q.Stop)

	tel := telemetry.NewMonitor(store, 1*time.Minute)
	dash := dashboard.NewDashboard(store, authMgr)

	srv := NewServer("127.0.0.1:0", q, authMgr, tel, dash, &updater.Updater{}, opts...)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	return srv, authMgr, &printCount
}

func wsURL(srv *Server) string {
	u := url.URL{Scheme: "ws", Host: srv.Addr(), Path: "/ws"}
	return u.String()
}

// dialWS opens a WebSocket, sending origin as the Origin header when non-empty.
func dialWS(t *testing.T, srv *Server, origin string) *websocket.Conn {
	t.Helper()

	var header http.Header
	if origin != "" {
		header = http.Header{"Origin": []string{origin}}
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv), header)
	if err != nil {
		t.Fatalf("failed to dial websocket with origin %q: %v", origin, err)
	}
	return conn
}

// sendWS performs one request/response round trip.
//
// An authenticated client also receives unsolicited push events on the same socket
// (job_status_changed, printer_status_changed), so anything that is not the response
// to this request is skipped rather than mistaken for it.
func sendWS(t *testing.T, conn *websocket.Conn, req WSRequest) WSResponse {
	t.Helper()

	if err := conn.WriteJSON(req); err != nil {
		t.Fatalf("write %s error: %v", req.Type, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			t.Fatalf("failed to set read deadline: %v", err)
		}

		var resp WSResponse
		if err := conn.ReadJSON(&resp); err != nil {
			t.Fatalf("read %s response error: %v", req.Type, err)
		}

		if resp.Type == "response" && resp.ID == req.ID {
			_ = conn.SetReadDeadline(time.Time{})
			return resp
		}
	}
}

func TestPairingCodeIsNotReturnedOverWebSocket(t *testing.T) {
	srv, authMgr, _ := newTestServer(t)

	conn := dialWS(t, srv, "")
	defer conn.Close()

	resp := sendWS(t, conn, WSRequest{
		Version: 1,
		ID:      "req_pairing",
		Type:    "request_pairing",
		Payload: map[string]any{"app_name": "Untrusted Page"},
	})

	if !resp.Success {
		t.Fatalf("request_pairing failed: %s", resp.Error)
	}

	payload, _ := resp.Payload.(map[string]any)
	if _, leaked := payload["code"]; leaked {
		t.Fatalf("pairing code was returned to the requester: %v", payload)
	}
	if requested, _ := payload["code_requested"].(bool); !requested {
		t.Errorf("expected code_requested=true in payload, got %v", payload)
	}
	if _, _, ok := authMgr.ActivePairingCode(); !ok {
		t.Error("expected the agent to hold a pairing code for the tray/dashboard to display")
	}
}

func TestDisallowedOriginIsRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv), http.Header{"Origin": []string{"https://evil.example"}})
	if err == nil {
		t.Fatal("expected the handshake from a disallowed origin to fail")
	}
	if resp == nil {
		t.Fatal("expected an HTTP response describing the origin rejection")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected HTTP 403 for a disallowed origin, got %d", resp.StatusCode)
	}
}

func TestAllowedOriginsOption(t *testing.T) {
	srv, _, _ := newTestServer(t,
		WithAllowedOrigins("https://pos.example.com", "http://pos.local:*"),
	)

	// Exact allowlisted origin.
	conn := dialWS(t, srv, "https://pos.example.com")
	if resp := sendWS(t, conn, WSRequest{Version: 1, ID: "p1", Type: "ping"}); !resp.Success {
		t.Errorf("ping from allowlisted origin failed: %s", resp.Error)
	}
	_ = conn.Close()

	// Allowlisted host on a different port.
	conn = dialWS(t, srv, "http://pos.local:8080")
	if resp := sendWS(t, conn, WSRequest{Version: 1, ID: "p2", Type: "ping"}); !resp.Success {
		t.Errorf("ping from wildcard-port origin failed: %s", resp.Error)
	}
	_ = conn.Close()

	// Loopback stays usable even when an explicit allowlist is configured.
	conn = dialWS(t, srv, "http://localhost:5173")
	if resp := sendWS(t, conn, WSRequest{Version: 1, ID: "p3", Type: "ping"}); !resp.Success {
		t.Errorf("ping from loopback origin failed: %s", resp.Error)
	}
	_ = conn.Close()

	// Anything else is still refused.
	if _, _, err := websocket.DefaultDialer.Dial(wsURL(srv), http.Header{"Origin": []string{"https://evil.example"}}); err == nil {
		t.Error("expected handshake from a non-allowlisted origin to fail")
	}
}

// TestPrivilegedMessagesRequireAuthentication proves that an unpaired page cannot
// enumerate printers, kick a cash drawer, or trigger an agent update.
func TestPrivilegedMessagesRequireAuthentication(t *testing.T) {
	srv, _, printCount := newTestServer(t)

	conn := dialWS(t, srv, "")
	defer conn.Close()

	privileged := []WSRequest{
		{Version: 1, ID: "m1", Type: "list_printers"},
		{Version: 1, ID: "m2", Type: "get_printer_health"},
		{Version: 1, ID: "m3", Type: "job_status", Payload: map[string]any{"job_id": "does-not-exist"}},
		{Version: 1, ID: "m4", Type: "open_drawer", Payload: map[string]any{"printer": "MockPrinter", "pin": 2}},
		{Version: 1, ID: "m5", Type: "list_serial_ports"},
		{Version: 1, ID: "m6", Type: "list_hid_devices"},
		{Version: 1, ID: "m7", Type: "check_update"},
		{Version: 1, ID: "m8", Type: "apply_update"},
		{Version: 1, ID: "m9", Type: "print", Payload: map[string]any{"printer": "MockPrinter", "data": "SGVsbG8="}},
		{Version: 1, ID: "m10", Type: "diagnose_printers"},
		{Version: 1, ID: "m11", Type: "list_printer_roles"},
		{Version: 1, ID: "m12", Type: "assign_printer_role", Payload: map[string]any{"role": "kitchen", "printer": "MockPrinter"}},
		{Version: 1, ID: "m13", Type: "remove_printer_role", Payload: map[string]any{"role": "kitchen"}},
		{Version: 1, ID: "m14", Type: "test_print", Payload: map[string]any{"printer": "MockPrinter"}},
		{Version: 1, ID: "m15", Type: "diagnose_printer", Payload: map[string]any{"printer": "MockPrinter"}},
		{Version: 1, ID: "m16", Type: "queue_status"},
		{Version: 1, ID: "m17", Type: "pause_queue"},
		{Version: 1, ID: "m18", Type: "resume_queue"},
		{Version: 1, ID: "m19", Type: "retry_job", Payload: map[string]any{"job_id": "x"}},
		{Version: 1, ID: "m20", Type: "clear_failed_jobs"},
	}

	for _, req := range privileged {
		resp := sendWS(t, conn, req)

		if resp.Success {
			t.Errorf("%s must require authentication, got success: %+v", req.Type, resp.Payload)
		}
		if resp.Error != "pairing_required" {
			t.Errorf("%s: expected error %q, got %q", req.Type, "pairing_required", resp.Error)
		}
		if resp.ID != req.ID {
			t.Errorf("%s: expected response id %q, got %q", req.Type, req.ID, resp.ID)
		}
	}

	if got := atomic.LoadInt32(printCount); got != 0 {
		t.Errorf("unauthenticated requests produced %d print side effect(s), want 0", got)
	}
}

// TestPingWorksBeforePairing keeps the documented pre-auth surface honest.
func TestPingWorksBeforePairing(t *testing.T) {
	srv, _, _ := newTestServer(t)

	conn := dialWS(t, srv, "")
	defer conn.Close()

	resp := sendWS(t, conn, WSRequest{Version: 1, ID: "ping_pre_auth", Type: "ping"})
	if !resp.Success {
		t.Fatalf("ping should work before pairing, got: %s", resp.Error)
	}

	payload, _ := resp.Payload.(map[string]any)
	if status, _ := payload["status"].(string); status != "pong" {
		t.Errorf("expected status pong, got %v", payload)
	}
}

// --- Health endpoint (P1) ---

func TestHealthEndpoint(t *testing.T) {
	srv, _, _ := newTestServer(t, WithAgentVersion("1.2.3"))

	res, err := http.Get("http://" + srv.Addr() + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer res.Body.Close()

	// 503 is expected on a machine with no printers: the agent is running but
	// cannot do its job, which is exactly the distinction /health must make.
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status %d", res.StatusCode)
	}

	var report struct {
		Service  string   `json:"service"`
		Version  string   `json:"version"`
		Status   string   `json:"status"`
		Reasons  []string `json:"reasons"`
		Printers []any    `json:"printers"`
		Queue    struct {
			Total int `json:"total"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(res.Body).Decode(&report); err != nil {
		t.Fatalf("failed to decode /health JSON: %v", err)
	}

	if report.Service != "printbridge-agent" {
		t.Errorf("expected service printbridge-agent, got %q", report.Service)
	}
	if report.Version != "1.2.3" {
		t.Errorf("expected version 1.2.3, got %q", report.Version)
	}
	if report.Status == "" || len(report.Reasons) == 0 {
		t.Errorf("expected a status and at least one reason, got %+v", report)
	}
	if report.Printers == nil {
		t.Error("expected printers to serialise as an array, not null")
	}

	// Status and HTTP code must agree.
	if report.Status == "unhealthy" && res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unhealthy report must answer 503, got %d", res.StatusCode)
	}
}

func TestHealthRejectsForeignHostHeader(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/health", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	// Simulates a DNS-rebinding attempt: the browser believes it is talking to
	// evil.example, which now resolves to this machine.
	req.Host = "evil.example:9567"

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusForbidden {
		t.Errorf("expected HTTP 403 for a foreign Host header, got %d", res.StatusCode)
	}
}

// --- Printer roles (P1.6) ---

// TestPrinterRoleEndToEnd walks the whole role flow over a real WebSocket: pair,
// assign a station to a printer, print and diagnose by role name, then remove it.
func TestPrinterRoleEndToEnd(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	authMgr, err := auth.NewManager(store)
	if err != nil {
		t.Fatalf("failed to create auth manager: %v", err)
	}

	// A fake printer list keeps this test independent of the machine's real queues,
	// and the server and the role manager are given the same view.
	fakeList := func() ([]printer.PrinterInfo, error) {
		return []printer.PrinterInfo{{
			Name:              "Station A",
			DriverName:        "Generic / Text Only",
			PortName:          "USB001",
			IsOnline:          true,
			State:             string(printer.StateOnline),
			StatusDescription: "Ready",
			Type:              "local",
		}}, nil
	}

	roleMgr := roles.NewManager(store)
	roleMgr.SetListFunc(fakeList)

	printed := make(chan string, 4)
	q, err := queue.NewQueue(store, queue.WithPrintFunc(func(printerName string, data []byte, jobName string) error {
		printed <- printerName
		return nil
	}))
	if err != nil {
		t.Fatalf("failed to create queue: %v", err)
	}
	defer q.Stop()

	tel := telemetry.NewMonitor(store, time.Minute)
	dash := dashboard.NewDashboard(store, authMgr)

	srv := NewServer("127.0.0.1:0", q, authMgr, tel, dash, &updater.Updater{},
		WithRoleManager(roleMgr),
		WithPrinterLister(fakeList),
	)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop()

	conn := dialWS(t, srv, "")
	defer conn.Close()

	// Pair the way a real client must: the code is read from the tray/dashboard.
	if resp := sendWS(t, conn, WSRequest{
		Version: 1, ID: "pair_req", Type: "request_pairing",
		Payload: map[string]any{"app_name": "Roles Test"},
	}); !resp.Success {
		t.Fatalf("request_pairing failed: %s", resp.Error)
	}
	code, _, ok := authMgr.ActivePairingCode()
	if !ok {
		t.Fatal("expected an active pairing code")
	}
	if resp := sendWS(t, conn, WSRequest{
		Version: 1, ID: "pair_confirm", Type: "confirm_pairing",
		Payload: map[string]any{"code": code},
	}); !resp.Success {
		t.Fatalf("confirm_pairing failed: %s", resp.Error)
	}

	// Assign the kitchen station to a printer, with a 58mm capability profile.
	assignResp := sendWS(t, conn, WSRequest{
		Version: 1, ID: "assign", Type: "assign_printer_role",
		Payload: map[string]any{
			"role":         "Kitchen",
			"printer":      "Station A",
			"label":        "Kitchen printer",
			"capabilities": map[string]any{"max_width": 32},
		},
	})
	if !assignResp.Success {
		t.Fatalf("assign_printer_role failed: %s", assignResp.Error)
	}
	assignPayload, _ := assignResp.Payload.(map[string]any)
	if assignPayload["role"] != "kitchen" {
		t.Errorf("expected the role name to be normalised, got %v", assignPayload["role"])
	}

	// The role resolves against the live printer list.
	listResp := sendWS(t, conn, WSRequest{Version: 1, ID: "roles", Type: "list_printer_roles"})
	if !listResp.Success {
		t.Fatalf("list_printer_roles failed: %s", listResp.Error)
	}
	rolesPayload, _ := listResp.Payload.(map[string]any)
	roleList, _ := rolesPayload["roles"].([]any)
	if len(roleList) != 1 {
		t.Fatalf("expected one role, got %v", rolesPayload)
	}
	first, _ := roleList[0].(map[string]any)
	if resolved, _ := first["resolved"].(bool); !resolved {
		t.Errorf("expected the role to resolve: %v", first)
	}

	// A test print addressed to the role lands on the resolved printer.
	testResp := sendWS(t, conn, WSRequest{
		Version: 1, ID: "test_print", Type: "test_print",
		Payload: map[string]any{"role": "kitchen", "slip": "kitchen"},
	})
	if !testResp.Success {
		t.Fatalf("test_print failed: %s", testResp.Error)
	}
	testPayload, _ := testResp.Payload.(map[string]any)
	if testPayload["printer"] != "Station A" {
		t.Errorf("expected the role to resolve to Station A, got %v", testPayload["printer"])
	}
	if bytes, _ := testPayload["bytes"].(float64); bytes <= 0 {
		t.Errorf("expected a non-empty test slip, got %v", testPayload["bytes"])
	}

	select {
	case name := <-printed:
		if name != "Station A" {
			t.Errorf("expected the job to be sent to the resolved printer, got %q", name)
		}
	case <-time.After(5 * time.Second):
		jobID, _ := testPayload["job_id"].(string)
		job, jerr := q.GetJobStatus(jobID)
		t.Fatalf("timed out waiting for the role-addressed test print to be delivered (job=%+v err=%v)", job, jerr)
	}

	// The diagnosis names the role and the transport.
	diagResp := sendWS(t, conn, WSRequest{
		Version: 1, ID: "diagnose", Type: "diagnose_printer",
		Payload: map[string]any{"role": "kitchen"},
	})
	if !diagResp.Success {
		t.Fatalf("diagnose_printer failed: %s", diagResp.Error)
	}
	diag, _ := diagResp.Payload.(map[string]any)
	if diag["printer"] != "Station A" {
		t.Errorf("expected the diagnosis to resolve the role, got %v", diag["printer"])
	}
	checks, _ := diag["checks"].([]any)
	if len(checks) == 0 {
		t.Error("expected plain-language checks in the diagnosis")
	}
	joined := make([]string, 0, len(checks))
	for _, c := range checks {
		text, _ := c.(string)
		joined = append(joined, text)
	}
	if !strings.Contains(strings.Join(joined, " | "), "serves role(s): kitchen") {
		t.Errorf("expected the diagnosis to name the role, got %v", joined)
	}

	// Removing the role stops the POS addressing it, without touching the printer.
	if resp := sendWS(t, conn, WSRequest{
		Version: 1, ID: "remove", Type: "remove_printer_role",
		Payload: map[string]any{"role": "kitchen"},
	}); !resp.Success {
		t.Fatalf("remove_printer_role failed: %s", resp.Error)
	}

	afterRemove := sendWS(t, conn, WSRequest{
		Version: 1, ID: "test_after_remove", Type: "test_print",
		Payload: map[string]any{"role": "kitchen"},
	})
	if afterRemove.Success {
		t.Error("expected printing to a removed role to fail")
	}
	if !strings.Contains(afterRemove.Error, "not configured") {
		t.Errorf("expected a clear error for the removed role, got %q", afterRemove.Error)
	}
}
