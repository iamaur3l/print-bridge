package health

import (
	"fmt"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
)

// Service identifies this agent in a /health response, so a client probing a port
// range can tell the real agent from whatever else is listening.
const Service = "printbridge-agent"

// Status is the aggregate verdict.
type Status string

const (
	Healthy   Status = "healthy"
	Degraded  Status = "degraded"
	Unhealthy Status = "unhealthy"
)

// Report is the /health payload.
type Report struct {
	Service       string               `json:"service"`
	Version       string               `json:"version"`
	Status        Status               `json:"status"`
	Reasons       []string             `json:"reasons"`
	Port          int                  `json:"port"`
	UptimeSeconds int64                `json:"uptime_seconds"`
	SnapshotAgeMS int64                `json:"snapshot_age_ms,omitempty"`
	Printers      []printer.PrinterInfo `json:"printers"`
	Queue         queue.JobCounts      `json:"queue"`
}

// Input is everything needed to build a report.
type Input struct {
	Version     string
	Port        int
	StartedAt   time.Time
	Printers    []printer.PrinterInfo
	Queue       queue.JobCounts
	SnapshotAge time.Duration
	PrinterErr  error
	Now         time.Time
}

// Evaluate turns observations into a verdict plus plain-language reasons.
//
// This is deliberately not a liveness check: an agent that is running but cannot
// print anything is degraded, not healthy. Reasons are written to be read by a
// support engineer, in discovery order.
func Evaluate(printers []printer.PrinterInfo, counts queue.JobCounts) (Status, []string) {
	status := Healthy
	reasons := []string{}

	online := 0
	for _, p := range printers {
		if p.IsOnline {
			online++
		}
	}

	if len(printers) == 0 {
		status = Unhealthy
		reasons = append(reasons, "no printers were discovered on this machine")
	} else if online == 0 {
		status = Degraded
		reasons = append(reasons, "no printer is currently available")
	}

	// Name every printer that is not online, worst state first.
	for _, p := range printers {
		if p.IsOnline {
			continue
		}

		state := p.State
		if state == "" {
			state = "offline"
		}

		if status == Healthy {
			status = Degraded
		}

		reason := fmt.Sprintf("printer %q is %s: %s", p.Name, state, p.StatusDescription)
		if p.State == string(printer.StateUnknown) && p.StaleWorkOffline {
			reason += " (spooler flag not corroborated - a successful print will clear this)"
		}
		reasons = append(reasons, reason)
	}

	if counts.Failed > 0 {
		if status == Healthy {
			status = Degraded
		}
		reasons = append(reasons, fmt.Sprintf("%d job(s) are in the dead-letter state and need attention", counts.Failed))
	}

	pending := counts.Queued + counts.Retrying
	if status == Healthy {
		reasons = append(reasons, fmt.Sprintf("%d printer(s) ready, %d job(s) pending", online, pending))
	}

	return status, reasons
}

// Build assembles the full report.
func Build(in Input) Report {
	if in.Now.IsZero() {
		in.Now = time.Now()
	}

	printers := in.Printers
	if printers == nil {
		printers = []printer.PrinterInfo{}
	}

	status, reasons := Evaluate(printers, in.Queue)

	if in.PrinterErr != nil {
		if status == Healthy {
			status = Degraded
		}
		reasons = append(reasons, fmt.Sprintf("printer enumeration failed: %v", in.PrinterErr))
	}

	report := Report{
		Service:       Service,
		Version:       in.Version,
		Status:        status,
		Reasons:       reasons,
		Port:          in.Port,
		Printers:      printers,
		Queue:         in.Queue,
		SnapshotAgeMS: in.SnapshotAge.Milliseconds(),
	}

	if !in.StartedAt.IsZero() {
		report.UptimeSeconds = int64(in.Now.Sub(in.StartedAt).Seconds())
	}

	return report
}
