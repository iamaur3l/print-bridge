package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/printbridge/printbridge/agent/pkg/auth"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

func TestDashboardStaticServing(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	authMgr, err := auth.NewManager(store)
	if err != nil {
		t.Fatalf("failed to create auth manager: %v", err)
	}

	dash := NewDashboard(store, authMgr)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()

	dash.ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Errorf("expected status 200 OK for /dashboard, got %d", res.StatusCode)
	}

	contentType := res.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Errorf("expected text/html content-type, got %s", contentType)
	}
}

func TestDashboardAPISummary(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	authMgr, err := auth.NewManager(store)
	if err != nil {
		t.Fatalf("failed to create auth manager: %v", err)
	}

	dash := NewDashboard(store, authMgr)

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard/summary", nil)
	w := httptest.NewRecorder()

	dash.ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", res.StatusCode)
	}

	var summary map[string]any
	if err := json.NewDecoder(res.Body).Decode(&summary); err != nil {
		t.Fatalf("failed to decode JSON summary: %v", err)
	}

	if summary["printers"] == nil || summary["jobs"] == nil {
		t.Errorf("expected printers and jobs in summary response: %+v", summary)
	}
}

func TestDashboardNewPairingCode(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	authMgr, err := auth.NewManager(store)
	if err != nil {
		t.Fatalf("failed to create auth manager: %v", err)
	}

	dash := NewDashboard(store, authMgr)

	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/pairing-code", nil)
	w := httptest.NewRecorder()

	dash.ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", res.StatusCode)
	}

	var resp map[string]any
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	code, ok := resp["code"].(string)
	if !ok || len(code) != 6 {
		t.Errorf("expected 6-digit pairing code, got: %+v", resp)
	}
}
