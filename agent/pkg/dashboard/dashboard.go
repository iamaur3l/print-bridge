package dashboard

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/printbridge/printbridge/agent/pkg/auth"
	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

//go:embed static/*
var staticFS embed.FS

type Dashboard struct {
	store *queue.Store
	auth  *auth.Manager
}

func NewDashboard(store *queue.Store, authMgr *auth.Manager) *Dashboard {
	return &Dashboard{
		store: store,
		auth:  authMgr,
	}
}

func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if strings.HasPrefix(path, "/api/dashboard/summary") {
		d.handleSummary(w, r)
		return
	}

	if strings.HasPrefix(path, "/api/dashboard/pairing-code") && r.Method == http.MethodPost {
		d.handleNewPairingCode(w, r)
		return
	}

	if strings.HasPrefix(path, "/api/dashboard/revoke") && r.Method == http.MethodPost {
		d.handleRevokeApp(w, r)
		return
	}

	// Serve static index.html dashboard UI
	indexBytes, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "Dashboard UI not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexBytes)
}

func (d *Dashboard) handleSummary(w http.ResponseWriter, r *http.Request) {
	printers, err := printer.ListPrintersCached()
	if err != nil || printers == nil {
		printers = []printer.PrinterInfo{}
	}

	jobs, err := d.store.ListRecentJobs(50)
	if err != nil || jobs == nil {
		jobs = []*queue.Job{}
	}

	pairedApps, err := d.auth.ListPairedApps()
	if err != nil || pairedApps == nil {
		pairedApps = []*auth.PairedApp{}
	}

	code, _, _ := d.auth.ActivePairingCode()

	resp := map[string]any{
		"printers":     printers,
		"jobs":         jobs,
		"paired_apps":  pairedApps,
		"pairing_code": code,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (d *Dashboard) handleNewPairingCode(w http.ResponseWriter, r *http.Request) {
	code, expiresAt, err := d.auth.RequestPairing("Dashboard UI", "http://localhost")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":       code,
		"expires_at": expiresAt,
	})
}

func (d *Dashboard) handleRevokeApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AppID string `json:"app_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AppID == "" {
		http.Error(w, "app_id is required", http.StatusBadRequest)
		return
	}

	if err := d.auth.RevokeApp(req.AppID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
}
