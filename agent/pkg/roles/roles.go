// Package roles maps stable job stations ("receipt", "kitchen") onto whatever
// physical printer currently serves them, so replacing hardware never requires a
// change in the POS.
package roles

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

// Errors returned by the manager.
var (
	ErrRoleNotFound  = errors.New("printer role is not configured")
	ErrRoleRequired  = errors.New("role name is required")
	ErrPrinterNeeded = errors.New("printer name is required")
)

// Role is a station assignment: a POS addresses the role, not the device.
type Role struct {
	Role         string               `json:"role"`
	PrinterName  string               `json:"printer_name"`
	Label        string               `json:"label,omitempty"`
	Capabilities printer.Capabilities `json:"capabilities"`
	CreatedAt    time.Time            `json:"created_at"`
	UpdatedAt    time.Time            `json:"updated_at"`
}

// Status is a role plus how it resolves right now.
type Status struct {
	Role         string               `json:"role"`
	PrinterName  string               `json:"printer_name"`
	Label        string               `json:"label,omitempty"`
	Capabilities printer.Capabilities `json:"capabilities"`
	// Resolved is false when the assigned printer is no longer installed.
	Resolved bool `json:"resolved"`
	// State/Status/Detail are copied from the live printer when it resolves.
	State  string `json:"state,omitempty"`
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Reason explains an unresolved role.
	Reason string `json:"reason,omitempty"`
}

// Manager stores role assignments and resolves them against the live printer list.
type Manager struct {
	store *queue.Store
	list  func() ([]printer.PrinterInfo, error)
	now   func() time.Time
}

// NewManager builds a Manager backed by the agent's SQLite store.
func NewManager(store *queue.Store) *Manager {
	return &Manager{
		store: store,
		list:  printer.ListPrintersCached,
		now:   time.Now,
	}
}

// SetListFunc overrides printer discovery (used by tests).
func (m *Manager) SetListFunc(f func() ([]printer.PrinterInfo, error)) {
	if f != nil {
		m.list = f
	}
}

// NormaliseRole trims and lower-cases a role name, so "Kitchen" and "kitchen" are
// the same station.
func NormaliseRole(role string) string {
	return strings.ToLower(strings.TrimSpace(role))
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRole(row rowScanner) (*Role, error) {
	var assigned Role
	var capsJSON, createdStr, updatedStr string

	if err := row.Scan(&assigned.Role, &assigned.PrinterName, &assigned.Label, &capsJSON, &createdStr, &updatedStr); err != nil {
		return nil, err
	}

	if capsJSON != "" {
		if err := json.Unmarshal([]byte(capsJSON), &assigned.Capabilities); err != nil {
			// A profile written by an older version must not make the role unusable.
			assigned.Capabilities = printer.DefaultCapabilities()
		}
	}
	assigned.Capabilities = assigned.Capabilities.WithDefaults()

	assigned.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	assigned.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)

	return &assigned, nil
}

// Assign points a role at a printer, creating or updating the assignment.
func (m *Manager) Assign(role, printerName, label string, caps printer.Capabilities) (*Role, error) {
	role = NormaliseRole(role)
	if role == "" {
		return nil, ErrRoleRequired
	}

	printerName = strings.TrimSpace(printerName)
	if printerName == "" {
		return nil, ErrPrinterNeeded
	}

	existing, err := m.Get(role)
	if err != nil && !errors.Is(err, ErrRoleNotFound) {
		return nil, err
	}

	now := m.now()
	assigned := &Role{
		Role:         role,
		PrinterName:  printerName,
		Label:        strings.TrimSpace(label),
		Capabilities: caps.WithDefaults(),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if existing != nil {
		assigned.CreatedAt = existing.CreatedAt
	}

	capsJSON, err := json.Marshal(assigned.Capabilities)
	if err != nil {
		return nil, fmt.Errorf("failed to encode capabilities: %w", err)
	}

	query := `
	INSERT INTO printer_roles (role, printer_name, label, capabilities, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(role) DO UPDATE SET
		printer_name = excluded.printer_name,
		label = excluded.label,
		capabilities = excluded.capabilities,
		updated_at = excluded.updated_at
	`
	if _, err := m.store.DB().Exec(query,
		assigned.Role,
		assigned.PrinterName,
		assigned.Label,
		string(capsJSON),
		assigned.CreatedAt.Format(time.RFC3339),
		assigned.UpdatedAt.Format(time.RFC3339),
	); err != nil {
		return nil, fmt.Errorf("failed to save printer role: %w", err)
	}

	_ = m.store.LogAuditEvent(queue.AuditRoleAssigned, "", "",
		fmt.Sprintf("role %q assigned to printer %q", assigned.Role, assigned.PrinterName), "")

	return assigned, nil
}

// Remove deletes a role assignment.
func (m *Manager) Remove(role string) error {
	role = NormaliseRole(role)
	if role == "" {
		return ErrRoleRequired
	}

	res, err := m.store.DB().Exec(`DELETE FROM printer_roles WHERE role = ?`, role)
	if err != nil {
		return fmt.Errorf("failed to remove printer role: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrRoleNotFound
	}

	_ = m.store.LogAuditEvent(queue.AuditRoleRemoved, "", "", fmt.Sprintf("role %q removed", role), "")
	return nil
}

// Get returns a single role.
func (m *Manager) Get(role string) (*Role, error) {
	role = NormaliseRole(role)
	if role == "" {
		return nil, ErrRoleRequired
	}

	row := m.store.DB().QueryRow(
		`SELECT role, printer_name, COALESCE(label, ''), COALESCE(capabilities, ''), created_at, updated_at
		 FROM printer_roles WHERE role = ?`, role)

	assigned, err := scanRole(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRoleNotFound
	}
	return assigned, err
}

// List returns every configured role.
func (m *Manager) List() ([]Role, error) {
	rows, err := m.store.DB().Query(
		`SELECT role, printer_name, COALESCE(label, ''), COALESCE(capabilities, ''), created_at, updated_at
		 FROM printer_roles ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	assignedRoles := make([]Role, 0, 4)
	for rows.Next() {
		assigned, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		assignedRoles = append(assignedRoles, *assigned)
	}

	return assignedRoles, rows.Err()
}

// Resolve returns the printer a role points at right now.
func (m *Manager) Resolve(role string) (printer.PrinterInfo, error) {
	assigned, err := m.Get(role)
	if err != nil {
		return printer.PrinterInfo{}, err
	}

	printers, err := m.list()
	if err != nil {
		return printer.PrinterInfo{}, fmt.Errorf("failed to list printers while resolving role %q: %w", assigned.Role, err)
	}

	for _, p := range printers {
		if strings.EqualFold(p.Name, assigned.PrinterName) {
			return p, nil
		}
	}

	return printer.PrinterInfo{}, fmt.Errorf("printer %q assigned to role %q is not installed", assigned.PrinterName, assigned.Role)
}

// Statuses renders every role with its live resolution, which is what the dashboard
// and the list_printer_roles request show.
func (m *Manager) Statuses() ([]Status, error) {
	assignedRoles, err := m.List()
	if err != nil {
		return nil, err
	}

	printers, _ := m.list()
	byName := make(map[string]printer.PrinterInfo, len(printers))
	for _, p := range printers {
		byName[strings.ToLower(p.Name)] = p
	}

	statuses := make([]Status, 0, len(assignedRoles))
	for _, assigned := range assignedRoles {
		status := Status{
			Role:         assigned.Role,
			PrinterName:  assigned.PrinterName,
			Label:        assigned.Label,
			Capabilities: assigned.Capabilities,
		}

		if live, ok := byName[strings.ToLower(assigned.PrinterName)]; ok {
			status.Resolved = true
			status.State = live.State
			status.Status = live.StatusDescription
			status.Detail = live.StatusDetail
		} else {
			status.Reason = fmt.Sprintf("printer %q is not installed", assigned.PrinterName)
		}

		statuses = append(statuses, status)
	}

	return statuses, nil
}

// RolesForPrinter reports which roles point at a printer (used by diagnostics).
func (m *Manager) RolesForPrinter(printerName string) ([]string, error) {
	assignedRoles, err := m.List()
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, 2)
	for _, assigned := range assignedRoles {
		if strings.EqualFold(assigned.PrinterName, printerName) {
			names = append(names, assigned.Role)
		}
	}
	return names, nil
}