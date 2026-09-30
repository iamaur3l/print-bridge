package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

var (
	ErrPairingExpired     = errors.New("pairing code expired or invalid")
	ErrMaxAttemptsExceeded = errors.New("too many failed code attempts; please request a new code")
	ErrAppNotPaired       = errors.New("application origin is not paired")
	ErrInvalidToken       = errors.New("invalid or revoked authentication token")
)

type PairedApp struct {
	ID         string    `json:"id"`
	AppName    string    `json:"app_name"`
	Origin     string    `json:"origin"`
	Token      string    `json:"token,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

type PendingPairing struct {
	Code      string
	AppName   string
	Origin    string
	ExpiresAt time.Time
	Attempts  int
}

type Manager struct {
	store      *queue.Store
	mu         sync.Mutex
	pendingMap map[string]*PendingPairing // key = origin
	hmacSecret []byte
}

func NewManager(store *queue.Store) (*Manager, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("failed to generate random HMAC secret: %w", err)
	}

	return &Manager{
		store:      store,
		pendingMap: make(map[string]*PendingPairing),
		hmacSecret: secret,
	}, nil
}

// ActivePairingCode returns the active 6-digit pairing code for display in UI/tray.
func (m *Manager) ActivePairingCode() (string, time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for _, p := range m.pendingMap {
		if now.Before(p.ExpiresAt) {
			return p.Code, p.ExpiresAt, true
		}
	}
	return "", time.Time{}, false
}

// RequestPairing initiates a pairing session and returns a 6-digit code.
func (m *Manager) RequestPairing(appName, origin string) (string, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if appName == "" {
		appName = "Web Application"
	}
	if origin == "" {
		origin = "http://localhost"
	}

	// Generate secure 6-digit numeric code (100000 - 999999)
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to generate random code: %w", err)
	}
	code := fmt.Sprintf("%06d", n.Int64()+100000)

	expiresAt := time.Now().Add(5 * time.Minute)
	m.pendingMap[origin] = &PendingPairing{
		Code:      code,
		AppName:   appName,
		Origin:    origin,
		ExpiresAt: expiresAt,
		Attempts:  0,
	}

	log.Printf("[Auth] New pairing request for %q (%s). Code: %s (expires in 5m)", appName, origin, code)

	_ = m.store.LogAuditEvent(queue.AuditPairingRequested, origin, appName, "pairing code generated", "")

	return code, expiresAt, nil
}

// ConfirmPairing validates the 6-digit code and issues a persistent token.
func (m *Manager) ConfirmPairing(code, origin string) (*PairedApp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pending, exists := m.pendingMap[origin]
	if !exists || time.Now().After(pending.ExpiresAt) {
		return nil, ErrPairingExpired
	}

	pending.Attempts++
	if pending.Attempts > 5 {
		delete(m.pendingMap, origin)
		return nil, ErrMaxAttemptsExceeded
	}

	if pending.Code != code {
		return nil, fmt.Errorf("incorrect pairing code (%d/5 attempts used)", pending.Attempts)
	}

	delete(m.pendingMap, origin)

	// Issue long-lived HMAC token
	appID := uuid.New().String()
	token := m.generateToken(appID, origin)

	now := time.Now()
	paired := &PairedApp{
		ID:         appID,
		AppName:    pending.AppName,
		Origin:     origin,
		Token:      token,
		CreatedAt:  now,
		LastUsedAt: now,
	}

	query := `
	INSERT INTO paired_apps (id, app_name, origin, token, created_at, last_used_at)
	VALUES (?, ?, ?, ?, ?, ?)
	`
	_, err := m.store.DB().Exec(query,
		paired.ID,
		paired.AppName,
		paired.Origin,
		paired.Token,
		paired.CreatedAt.Format(time.RFC3339),
		paired.LastUsedAt.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to save paired app: %w", err)
	}

	log.Printf("[Auth] App %q (%s) paired successfully!", paired.AppName, origin)
	_ = m.store.LogAuditEvent(queue.AuditPairingConfirmed, origin, paired.AppName, "pairing completed and token issued", "")

	return paired, nil
}

// ValidateToken verifies that a token is valid and matches the request origin.
func (m *Manager) ValidateToken(token, origin string) (bool, *PairedApp) {
	if token == "" {
		return false, nil
	}

	query := `
	SELECT id, app_name, origin, token, created_at, last_used_at
	FROM paired_apps WHERE token = ?
	`
	row := m.store.DB().QueryRow(query, token)

	var app PairedApp
	var createdAtStr, lastUsedStr string

	err := row.Scan(&app.ID, &app.AppName, &app.Origin, &app.Token, &createdAtStr, &lastUsedStr)
	if err != nil {
		_ = m.store.LogAuditEvent(queue.AuditTokenRejected, origin, "", "invalid token", "")
		return false, nil
	}

	// A token is only valid for the origin it was issued to. This stops a token
	// handed to one web application from being replayed by any other origin that
	// can reach the loopback agent.
	if app.Origin != origin {
		_ = m.store.LogAuditEvent(queue.AuditTokenRejected, origin, app.AppName,
			fmt.Sprintf("token issued to origin %q presented from origin %q", app.Origin, origin), "")
		return false, nil
	}

	_ = m.store.LogAuditEvent(queue.AuditTokenValidated, origin, app.AppName, "token validated successfully", "")

	// Update last_used_at timestamp asynchronously
	now := time.Now()
	go func() {
		_, _ = m.store.DB().Exec("UPDATE paired_apps SET last_used_at = ? WHERE id = ?", now.Format(time.RFC3339), app.ID)
	}()

	return true, &app
}

// RevokeApp revokes access for a paired app ID.
func (m *Manager) RevokeApp(appID string) error {
	var appName, origin string
	_ = m.store.DB().QueryRow("SELECT app_name, origin FROM paired_apps WHERE id = ?", appID).Scan(&appName, &origin)

	_, err := m.store.DB().Exec("DELETE FROM paired_apps WHERE id = ?", appID)
	if err != nil {
		return err
	}

	_ = m.store.LogAuditEvent(queue.AuditAppRevoked, origin, appName, "app access revoked", "")
	return nil
}

// ListPairedApps returns all paired web applications.
func (m *Manager) ListPairedApps() ([]*PairedApp, error) {
	query := `SELECT id, app_name, origin, created_at, last_used_at FROM paired_apps ORDER BY created_at DESC`
	rows, err := m.store.DB().Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []*PairedApp
	for rows.Next() {
		var a PairedApp
		var createdAtStr, lastUsedStr sql.NullString
		if err := rows.Scan(&a.ID, &a.AppName, &a.Origin, &createdAtStr, &lastUsedStr); err != nil {
			return nil, err
		}
		if createdAtStr.Valid {
			a.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr.String)
		}
		if lastUsedStr.Valid {
			a.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsedStr.String)
		}
		apps = append(apps, &a)
	}
	return apps, nil
}

func (m *Manager) generateToken(appID, origin string) string {
	mac := hmac.New(sha256.New, m.hmacSecret)
	mac.Write([]byte(fmt.Sprintf("%s:%s", appID, origin)))
	return fmt.Sprintf("pb_tok_%s_%s", appID[:8], hex.EncodeToString(mac.Sum(nil))[:32])
}
