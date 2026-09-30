//go:build windows

package service

import (
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Install registers the agent with the Service Control Manager.
//
// args are baked into the service command line, so options such as -config survive a
// reboot. An existing installation is removed first, which is exactly what a repair
// needs; the configuration file itself is never touched.
func Install(execPath string, args []string) error {
	if execPath == "" {
		var err error
		execPath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("failed to resolve the agent executable: %w", err)
		}
	}

	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to open the service manager (this needs administrator rights): %w", err)
	}
	defer manager.Disconnect()

	if existing, err := manager.OpenService(Name); err == nil {
		existing.Close()
		if err := Uninstall(); err != nil {
			return fmt.Errorf("failed to replace the existing service: %w", err)
		}
	}

	cfg := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        windows.SERVICE_AUTO_START,
		ErrorControl:     windows.SERVICE_ERROR_NORMAL,
		DisplayName:      DisplayName,
		Description:      Description,
		ServiceStartName: "LocalSystem",
		Dependencies:     []string{SpoolerDependency},
	}

	installed, err := manager.CreateService(Name, execPath, cfg, args...)
	if err != nil {
		return fmt.Errorf("failed to create the service: %w", err)
	}
	defer installed.Close()

	// Restart on failure, backing off, so a crash at 3am recovers by itself instead of
	// waiting for someone to walk into the restaurant.
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}
	if err := installed.SetRecoveryActions(actions, 86400); err != nil {
		log.Printf("[Service] Warning: could not configure failure recovery: %v", err)
	}

	return nil
}

// Uninstall stops and removes the service.
func Uninstall() error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to open the service manager (this needs administrator rights): %w", err)
	}
	defer manager.Disconnect()

	installed, err := manager.OpenService(Name)
	if err != nil {
		if isNotInstalled(err) {
			return nil
		}
		return fmt.Errorf("failed to open the service: %w", err)
	}
	defer installed.Close()

	// Stop first: the SCM marks a running service for deletion but keeps the process
	// alive until it exits, which surprises whoever runs the uninstall.
	_, _ = installed.Control(svc.Stop)
	waitForState(installed, svc.Stopped, 10*time.Second)

	if err := installed.Delete(); err != nil {
		return fmt.Errorf("failed to delete the service: %w", err)
	}
	return nil
}

// Start asks the SCM to start the service.
func Start() error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to open the service manager: %w", err)
	}
	defer manager.Disconnect()

	installed, err := manager.OpenService(Name)
	if err != nil {
		return fmt.Errorf("failed to open the service (is it installed?): %w", err)
	}
	defer installed.Close()

	if err := installed.Start(); err != nil {
		return fmt.Errorf("failed to start the service: %w", err)
	}
	return nil
}

// Stop asks the SCM to stop the service and waits for it to do so.
func Stop() error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to open the service manager: %w", err)
	}
	defer manager.Disconnect()

	installed, err := manager.OpenService(Name)
	if err != nil {
		if isNotInstalled(err) {
			return nil
		}
		return fmt.Errorf("failed to open the service: %w", err)
	}
	defer installed.Close()

	if _, err := installed.Control(svc.Stop); err != nil {
		return fmt.Errorf("failed to stop the service: %w", err)
	}
	waitForState(installed, svc.Stopped, 15*time.Second)
	return nil
}

// Query reports whether the service is installed and what state it is in.
//
// It uses a read-only handle on purpose: checking status must work for an operator
// who is not elevated (mgr.Connect asks for full access, which needs administrator
// rights and fails with "Access is denied").
func Query() (Status, error) {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return Status{}, fmt.Errorf("failed to open the service manager: %w", err)
	}
	defer windows.CloseServiceHandle(manager)

	namePtr, err := windows.UTF16PtrFromString(Name)
	if err != nil {
		return Status{}, fmt.Errorf("invalid service name %q: %w", Name, err)
	}

	service, err := windows.OpenService(manager, namePtr, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return Status{Installed: false, Detail: "not installed"}, nil
		}
		return Status{}, fmt.Errorf("failed to open the service: %w", err)
	}
	defer windows.CloseServiceHandle(service)

	var status windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(service, &status); err != nil {
		return Status{Installed: true}, fmt.Errorf("failed to query the service: %w", err)
	}

	// Only the state is reported: the process id lives in SERVICE_STATUS_PROCESS,
	// which needs a broader access right than an unelevated status check should ask
	// for.
	return Status{Installed: true, State: stateName(svc.State(status.CurrentState))}, nil
}

// Run starts the service control dispatcher and calls handler with a channel that is
// closed when the SCM asks the agent to stop. It returns once the agent has finished
// unwinding.
//
// Started outside the Service Control Manager this fails, which is the intended
// answer: `-service run` belongs to the SCM, and an operator uses the plain command.
func Run(handler func(stop <-chan struct{}) error) error {
	if err := svc.Run(Name, &serviceHandler{handler: handler}); err != nil {
		return fmt.Errorf("could not talk to the service manager (%w); start the agent without -service to run it in a console", err)
	}
	return nil
}

type serviceHandler struct {
	handler func(stop <-chan struct{}) error
}

func (h *serviceHandler) Execute(args []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}

	stop := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- h.handler(stop)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			// The agent finished on its own (a fatal error, most likely).
			changes <- svc.Status{State: svc.Stopped}
			if err != nil {
				log.Printf("[Service] Agent stopped with an error: %v", err)
				return true, 1
			}
			return false, 0

		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus

			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				close(stop)

				// Let the agent drain before telling the SCM the service has stopped.
				if err := <-done; err != nil {
					log.Printf("[Service] Agent stopped with an error: %v", err)
					return true, 1
				}
				changes <- svc.Status{State: svc.Stopped}
				return false, 0

			default:
				log.Printf("[Service] Ignoring unsupported control request %d", request.Cmd)
			}
		}
	}
}

// waitForState polls until the service reaches the wanted state or the deadline
// passes: SCM state changes are asynchronous, so a fixed sleep would be either flaky
// or slow.
func waitForState(service *mgr.Service, want svc.State, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		current, err := service.Query()
		if err != nil || current.State == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// isNotInstalled distinguishes "there is no service" from a real error, because the
// first is a normal answer for an uninstall or a status check.
func isNotInstalled(err error) bool {
	return errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST)
}

func stateName(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.Paused:
		return "paused"
	case svc.PausePending:
		return "pausing"
	case svc.ContinuePending:
		return "resuming"
	default:
		return fmt.Sprintf("unknown (%d)", state)
	}
}