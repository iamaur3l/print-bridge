package service

import (
	"strings"
	"testing"
)

func TestParseAction(t *testing.T) {
	cases := map[string]Action{
		"install":   ActionInstall,
		"INSTALL":   ActionInstall,
		" add":      ActionInstall,
		"repair":    ActionInstall,
		"uninstall": ActionUninstall,
		"remove":    ActionUninstall,
		"start":     ActionStart,
		"stop":      ActionStop,
		"status":    ActionStatus,
		"query":     ActionStatus,
		"run":       ActionRun,
	}

	for input, want := range cases {
		got, err := ParseAction(input)
		if err != nil {
			t.Errorf("ParseAction(%q) failed: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseAction(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseActionRejectsUnknownVerbs(t *testing.T) {
	for _, input := range []string{"", "restart", "delete"} {
		if _, err := ParseAction(input); err == nil {
			t.Errorf("ParseAction(%q) should have failed", input)
		} else if !strings.Contains(err.Error(), "expected install") {
			t.Errorf("expected a helpful error for %q, got %v", input, err)
		}
	}
}

// TestRunArgsKeepsTheConfigPath proves a service installation remembers where its
// configuration lives, which is what makes the settings survive a reboot.
func TestRunArgsKeepsTheConfigPath(t *testing.T) {
	args := RunArgs(`C:\ProgramData\PrintBridge\config.json`)

	if len(args) == 0 || args[0] != "-service" {
		t.Fatalf("expected the service verb first, got %v", args)
	}
	if !containsArg(args, "-headless") {
		t.Error("a service must run headless: there is no desktop in session 0")
	}
	if !containsArg(args, "-config") || !containsArg(args, `C:\ProgramData\PrintBridge\config.json`) {
		t.Errorf("expected the config path to be baked in, got %v", args)
	}

	// Without a config path the arguments are still valid.
	if bare := RunArgs(""); containsArg(bare, "-config") {
		t.Errorf("expected no config argument, got %v", bare)
	}
}

func TestStatusString(t *testing.T) {
	if got := (Status{Installed: false}).String(); got != "not installed" {
		t.Errorf("unexpected status text: %q", got)
	}
	if got := (Status{Installed: true, State: "running", Detail: "pid 42"}).String(); !strings.Contains(got, "running") || !strings.Contains(got, "pid 42") {
		t.Errorf("unexpected status text: %q", got)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}