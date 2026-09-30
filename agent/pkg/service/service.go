// Package service runs the agent under the Windows Service Control Manager and
// implements the install / uninstall / start / stop / status verbs.
//
// A Windows service runs in session 0 and has no desktop, so an agent started by the
// SCM always runs headless: the tray icon belongs to the interactive mode only.
package service

import (
	"errors"
	"fmt"
	"strings"
)

// Service identity in the Service Control Manager.
const (
	Name        = "printbridge-agent"
	DisplayName = "PrintBridge Agent"
	Description = "Local print & device bridge for web applications: queued raw printing, printer health, station roles."
	// SpoolerDependency makes Windows start the print spooler before the agent.
	SpoolerDependency = "Spooler"
)

// ErrUnsupported is returned by platforms without a service manager.
var ErrUnsupported = errors.New("service management is only supported on Windows")

// Action is a service control verb.
type Action string

const (
	ActionInstall   Action = "install"
	ActionUninstall Action = "uninstall"
	ActionStart     Action = "start"
	ActionStop      Action = "stop"
	ActionStatus    Action = "status"
	// ActionRun is how the Service Control Manager launches the agent. Started by
	// hand it fails, because there is no dispatcher to talk to.
	ActionRun Action = "run"
)

// ParseAction maps a command-line verb to an Action. "repair" is accepted as a
// synonym for install, because reinstalling over a half-finished install is the
// correct recovery and the installer needs a word for it.
func ParseAction(value string) (Action, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(ActionInstall), "add", "repair":
		return ActionInstall, nil
	case string(ActionUninstall), "remove":
		return ActionUninstall, nil
	case string(ActionStart):
		return ActionStart, nil
	case string(ActionStop):
		return ActionStop, nil
	case string(ActionStatus), "query":
		return ActionStatus, nil
	case string(ActionRun):
		return ActionRun, nil
	default:
		return "", fmt.Errorf("unknown service action %q (expected install, uninstall, start, stop, status or repair)", value)
	}
}

// RunArgs is the argument list the service is installed with, so that a
// configuration path survives a reboot.
func RunArgs(configPath string) []string {
	args := []string{"-service", string(ActionRun), "-headless"}
	if strings.TrimSpace(configPath) != "" {
		args = append(args, "-config", configPath)
	}
	return args
}

// Status reports what the service manager thinks of the agent.
type Status struct {
	Installed bool   `json:"installed"`
	State     string `json:"state,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// String renders a status for the console.
func (s Status) String() string {
	if !s.Installed {
		return "not installed"
	}
	if s.Detail != "" {
		return fmt.Sprintf("%s (%s)", s.State, s.Detail)
	}
	return s.State
}
