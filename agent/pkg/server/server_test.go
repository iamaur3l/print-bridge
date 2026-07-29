package server

import (
	"encoding/base64"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/printbridge/printbridge/agent/pkg/auth"
	"github.com/printbridge/printbridge/agent/pkg/dashboard"
	"github.com/printbridge/printbridge/agent/pkg/queue"
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
	code := payloadMap["code"].(string)

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
