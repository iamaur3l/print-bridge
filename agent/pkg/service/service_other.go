//go:build !windows

package service

// Install is unsupported off Windows.
func Install(execPath string, args []string) error { return ErrUnsupported }

// Uninstall is unsupported off Windows.
func Uninstall() error { return ErrUnsupported }

// Start is unsupported off Windows.
func Start() error { return ErrUnsupported }

// Stop is unsupported off Windows.
func Stop() error { return ErrUnsupported }

// Query reports that there is no service manager to ask.
func Query() (Status, error) {
	return Status{Installed: false, Detail: ErrUnsupported.Error()}, nil
}

// Run is unsupported off Windows; the caller should run the agent in the foreground
// (systemd and launchd are the service managers there).
func Run(handler func(stop <-chan struct{}) error) error { return ErrUnsupported }
