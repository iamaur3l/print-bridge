package health

import (
	"strings"
	"testing"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

func TestEvaluateNoPrintersIsUnhealthy(t *testing.T) {
	status, reasons := Evaluate(nil, queue.JobCounts{})
	if status != Unhealthy {
		t.Errorf("expected %q with no printers, got %q", Unhealthy, status)
	}
	if len(reasons) == 0 {
		t.Error("expected a reason explaining why the agent cannot work")
	}
}

func TestEvaluateNoAvailablePrinterIsDegraded(t *testing.T) {
	printers := []printer.PrinterInfo{{
		Name:              "Receipt",
		IsOnline:          false,
		State:             string(printer.StateOffline),
		StatusDescription: "Paper out",
	}}

	status, reasons := Evaluate(printers, queue.JobCounts{})
	if status != Degraded {
		t.Errorf("expected %q when no printer is available, got %q", Degraded, status)
	}

	joined := strings.Join(reasons, " | ")
	if !strings.Contains(joined, "no printer is currently available") {
		t.Errorf("expected an availability reason, got: %s", joined)
	}
	if !strings.Contains(joined, `printer "Receipt" is offline: Paper out`) {
		t.Errorf("expected the offending printer to be named, got: %s", joined)
	}
}

// TestEvaluateExplainsStaleSpoolerFlag is the health-layer half of the false
// "Offline" bug: an unverified printer must be reported as unknown with an
// explanation, never as a broken printer.
func TestEvaluateExplainsStaleSpoolerFlag(t *testing.T) {
	printers := []printer.PrinterInfo{{
		Name:              "USB Receipt",
		IsOnline:          false,
		State:             string(printer.StateUnknown),
		StatusDescription: "Status unverified (spooler reports offline)",
		StaleWorkOffline:  true,
	}}

	status, reasons := Evaluate(printers, queue.JobCounts{})
	if status != Degraded {
		t.Errorf("expected %q, got %q", Degraded, status)
	}

	joined := strings.Join(reasons, " | ")
	if !strings.Contains(joined, "is unknown") {
		t.Errorf("expected the unknown state to be surfaced, got: %s", joined)
	}
	if !strings.Contains(joined, "spooler flag not corroborated") {
		t.Errorf("expected the stale-flag explanation, got: %s", joined)
	}
}

func TestEvaluateHealthyWhenPrintersAreOnline(t *testing.T) {
	printers := []printer.PrinterInfo{
		{Name: "Receipt", IsOnline: true, State: string(printer.StateOnline), StatusDescription: "Ready"},
		{Name: "Kitchen", IsOnline: true, State: string(printer.StateOnline), StatusDescription: "Ready"},
	}

	status, reasons := Evaluate(printers, queue.JobCounts{Queued: 3})
	if status != Healthy {
		t.Errorf("expected %q, got %q (reasons: %v)", Healthy, status, reasons)
	}

	joined := strings.Join(reasons, " | ")
	if !strings.Contains(joined, "2 printer(s) ready") || !strings.Contains(joined, "3 job(s) pending") {
		t.Errorf("expected a healthy summary, got: %s", joined)
	}
}

func TestEvaluateFailedJobsDegrade(t *testing.T) {
	printers := []printer.PrinterInfo{
		{Name: "Receipt", IsOnline: true, State: string(printer.StateOnline), StatusDescription: "Ready"},
	}

	status, reasons := Evaluate(printers, queue.JobCounts{Failed: 2})
	if status != Degraded {
		t.Errorf("expected %q when jobs have failed permanently, got %q", Degraded, status)
	}
	if !strings.Contains(strings.Join(reasons, " | "), "dead-letter") {
		t.Errorf("expected a dead-letter reason, got: %v", reasons)
	}
}

func TestBuildFillsReportFields(t *testing.T) {
	started := time.Now().Add(-90 * time.Second)
	report := Build(Input{
		Version:     "9.9.9",
		Port:        9567,
		StartedAt:   started,
		Printers:    []printer.PrinterInfo{{Name: "Receipt", IsOnline: true, State: string(printer.StateOnline)}},
		Queue:       queue.JobCounts{Queued: 1},
		SnapshotAge: 2 * time.Second,
	})

	if report.Service != Service {
		t.Errorf("expected service %q, got %q", Service, report.Service)
	}
	if report.Version != "9.9.9" || report.Port != 9567 {
		t.Errorf("unexpected identity fields: %+v", report)
	}
	if report.UptimeSeconds < 90 {
		t.Errorf("expected uptime >= 90s, got %d", report.UptimeSeconds)
	}
	if report.SnapshotAgeMS != 2000 {
		t.Errorf("expected snapshot age 2000ms, got %d", report.SnapshotAgeMS)
	}

	// An empty printer list must serialise as [] rather than null.
	empty := Build(Input{})
	if empty.Printers == nil {
		t.Error("expected an empty (non-nil) printers slice")
	}
	if empty.Status != Unhealthy {
		t.Errorf("expected an agent with no printers to be %q, got %q", Unhealthy, empty.Status)
	}
}

func TestBuildReportsEnumerationFailure(t *testing.T) {
	report := Build(Input{
		Printers:   []printer.PrinterInfo{{Name: "Receipt", IsOnline: true, State: string(printer.StateOnline)}},
		PrinterErr: errEnumeration{},
	})

	if report.Status != Degraded {
		t.Errorf("expected %q when enumeration fails, got %q", Degraded, report.Status)
	}
	if !strings.Contains(strings.Join(report.Reasons, " | "), "printer enumeration failed") {
		t.Errorf("expected an enumeration-failure reason, got: %v", report.Reasons)
	}
}

type errEnumeration struct{}

func (errEnumeration) Error() string { return "lpstat not available" }