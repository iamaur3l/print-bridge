package roles

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

// newTestManager returns a manager over an in-memory store whose printer discovery
// can be swapped, so role resolution is tested without touching the real machine.
func newTestManager(t *testing.T) (*Manager, func(printers ...printer.PrinterInfo)) {
	t.Helper()

	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	manager := NewManager(store)

	live := []printer.PrinterInfo{}
	manager.SetListFunc(func() ([]printer.PrinterInfo, error) { return live, nil })

	return manager, func(printers ...printer.PrinterInfo) { live = printers }
}

func TestAssignAndResolve(t *testing.T) {
	manager, setLive := newTestManager(t)

	if _, err := manager.Assign("kitchen", "Kitchen Printer", "Kitchen", printer.Capabilities{}); err != nil {
		t.Fatalf("Assign failed: %v", err)
	}

	// Queue names vary in case between Windows versions and drivers.
	setLive(printer.PrinterInfo{Name: "kitchen printer", State: string(printer.StateOnline), StatusDescription: "Ready"})

	resolved, err := manager.Resolve("kitchen")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if resolved.Name != "kitchen printer" {
		t.Errorf("expected the live queue to be returned, got %+v", resolved)
	}
}

func TestAssignNormalisesRoleName(t *testing.T) {
	manager, _ := newTestManager(t)

	assigned, err := manager.Assign("  KitChen ", "Kitchen Printer", "", printer.Capabilities{})
	if err != nil {
		t.Fatalf("Assign failed: %v", err)
	}
	if assigned.Role != "kitchen" {
		t.Errorf("expected the role name to be normalised, got %q", assigned.Role)
	}

	if _, err := manager.Get("KITCHEN"); err != nil {
		t.Errorf("expected role lookup to be case-insensitive, got %v", err)
	}
}

func TestAssignValidation(t *testing.T) {
	manager, _ := newTestManager(t)

	if _, err := manager.Assign("", "Printer", "", printer.Capabilities{}); !errors.Is(err, ErrRoleRequired) {
		t.Errorf("expected ErrRoleRequired for an empty role, got %v", err)
	}
	if _, err := manager.Assign("kitchen", "", "", printer.Capabilities{}); !errors.Is(err, ErrPrinterNeeded) {
		t.Errorf("expected ErrPrinterNeeded for an empty printer, got %v", err)
	}
}

func TestAssignDefaultsAndPersistsCapabilities(t *testing.T) {
	manager, _ := newTestManager(t)

	// A profile with no width is completed with a usable one. Features that were not
	// claimed are left off: the agent must never invent a capability that would make
	// it emit escape sequences a printer cannot handle.
	if _, err := manager.Assign("receipt", "Receipt", "", printer.Capabilities{SupportsBold: true}); err != nil {
		t.Fatalf("Assign failed: %v", err)
	}
	stored, err := manager.Get("receipt")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if stored.Capabilities.MaxWidth != printer.Width80mm {
		t.Errorf("expected the width to default to %d, got %+v", printer.Width80mm, stored.Capabilities)
	}
	if !stored.Capabilities.SupportsBold {
		t.Errorf("expected a claimed capability to survive, got %+v", stored.Capabilities)
	}
	if stored.Capabilities.SupportsCut {
		t.Errorf("expected an unclaimed capability to stay off, got %+v", stored.Capabilities)
	}

	// A full default profile is available for callers that have no profile at all.
	if defaults := printer.DefaultCapabilities(); !defaults.SupportsCut || !defaults.SupportsCashDrawer {
		t.Errorf("unexpected defaults: %+v", defaults)
	}

	// An explicit profile survives a storage round trip.
	caps := printer.Capabilities{MaxWidth: printer.Width58mm, SupportsBold: true}
	if _, err := manager.Assign("bar", "Bar Printer", "Bar", caps); err != nil {
		t.Fatalf("Assign failed: %v", err)
	}
	stored, err = manager.Get("bar")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if stored.Capabilities.MaxWidth != printer.Width58mm || stored.Capabilities.SupportsCut {
		t.Errorf("capabilities did not round trip: %+v", stored.Capabilities)
	}
	if stored.Label != "Bar" {
		t.Errorf("expected the label to be stored, got %q", stored.Label)
	}
}

func TestAssignPreservesCreatedAt(t *testing.T) {
	manager, _ := newTestManager(t)

	// Timestamps are stored as RFC3339, so they carry second precision.
	first := time.Now().Add(-time.Hour).Truncate(time.Second)
	second := time.Now().Truncate(time.Second)
	calls := 0
	manager.now = func() time.Time {
		calls++
		if calls == 1 {
			return first
		}
		return second
	}

	created, err := manager.Assign("kitchen", "Old Printer", "", printer.Capabilities{})
	if err != nil {
		t.Fatalf("Assign failed: %v", err)
	}

	// Reassigning a role is how hardware gets replaced; the creation time is history.
	updated, err := manager.Assign("kitchen", "New Printer", "", printer.Capabilities{})
	if err != nil {
		t.Fatalf("Assign failed: %v", err)
	}

	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("expected CreatedAt to be preserved, got %v then %v", created.CreatedAt, updated.CreatedAt)
	}
	if updated.PrinterName != "New Printer" {
		t.Errorf("expected the assignment to move, got %q", updated.PrinterName)
	}
}

func TestRemoveRole(t *testing.T) {
	manager, _ := newTestManager(t)

	if err := manager.Remove("kitchen"); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected ErrRoleNotFound for an unknown role, got %v", err)
	}

	if _, err := manager.Assign("kitchen", "Kitchen Printer", "", printer.Capabilities{}); err != nil {
		t.Fatalf("Assign failed: %v", err)
	}
	if err := manager.Remove("KITCHEN"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	if _, err := manager.Get("kitchen"); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected the role to be gone, got %v", err)
	}
}

func TestResolveFailsWhenPrinterIsGone(t *testing.T) {
	manager, setLive := newTestManager(t)

	if _, err := manager.Assign("kitchen", "Ghost Printer", "", printer.Capabilities{}); err != nil {
		t.Fatalf("Assign failed: %v", err)
	}
	setLive()

	_, err := manager.Resolve("kitchen")
	if err == nil {
		t.Fatal("expected an error when the assigned printer is not installed")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("expected a helpful error, got %v", err)
	}
}

func TestStatusesReportResolution(t *testing.T) {
	manager, setLive := newTestManager(t)

	if _, err := manager.Assign("kitchen", "Kitchen Printer", "", printer.Capabilities{}); err != nil {
		t.Fatalf("Assign failed: %v", err)
	}
	if _, err := manager.Assign("receipt", "Missing Printer", "", printer.Capabilities{}); err != nil {
		t.Fatalf("Assign failed: %v", err)
	}

	setLive(printer.PrinterInfo{
		Name:              "Kitchen Printer",
		State:             string(printer.StateOnline),
		StatusDescription: "Ready",
		StatusDetail:      "port is present",
	})

	statuses, err := manager.Statuses()
	if err != nil {
		t.Fatalf("Statuses failed: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}

	// Ordered by role name: kitchen, receipt.
	kitchen, receipt := statuses[0], statuses[1]
	if kitchen.Role != "kitchen" || !kitchen.Resolved || kitchen.State != string(printer.StateOnline) {
		t.Errorf("unexpected kitchen status: %+v", kitchen)
	}
	if receipt.Role != "receipt" || receipt.Resolved {
		t.Errorf("expected the missing printer to be unresolved, got %+v", receipt)
	}
	if !strings.Contains(receipt.Reason, "not installed") {
		t.Errorf("expected an explanation for the unresolved role, got %q", receipt.Reason)
	}
}

func TestRolesForPrinter(t *testing.T) {
	manager, _ := newTestManager(t)

	for role, name := range map[string]string{"receipt": "Shared Printer", "kitchen": "shared printer"} {
		if _, err := manager.Assign(role, name, "", printer.Capabilities{}); err != nil {
			t.Fatalf("Assign failed: %v", err)
		}
	}
	if _, err := manager.Assign("bar", "Other Printer", "", printer.Capabilities{}); err != nil {
		t.Fatalf("Assign failed: %v", err)
	}

	names, err := manager.RolesForPrinter("SHARED PRINTER")
	if err != nil {
		t.Fatalf("RolesForPrinter failed: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 roles on the shared printer, got %v", names)
	}
	if names[0] != "kitchen" || names[1] != "receipt" {
		t.Errorf("expected role names in order, got %v", names)
	}
}