package tray

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/printbridge/printbridge/agent/pkg/autostart"
	"github.com/printbridge/printbridge/agent/pkg/queue"
	"github.com/printbridge/printbridge/agent/pkg/updater"
)

type State string

const (
	StateReady    State = "ready"
	StateOffline  State = "offline"
	StateError    State = "error"
	StatePrinting State = "printing"
)

type Tray struct {
	q           *queue.Queue
	serverPort  int
	updater     *updater.Updater
	currentState State
	mu          sync.Mutex
	mStatus     *systray.MenuItem
	mAutoLaunch *systray.MenuItem
	mUpdate     *systray.MenuItem
	onQuit      func()
}

func NewTray(q *queue.Queue, serverPort int, up *updater.Updater, onQuit func()) *Tray {
	return &Tray{
		q:           q,
		serverPort:  serverPort,
		updater:     up,
		currentState: StateReady,
		onQuit:      onQuit,
	}
}

// Run starts the system tray loop. Note: systray.Run blocks the main thread.
func (t *Tray) Run() {
	systray.Run(t.onReady, t.onExit)
}

func (t *Tray) onReady() {
	systray.SetTitle("PrintBridge")
	systray.SetTooltip("PrintBridge Local Agent")
	systray.SetIcon(getIconBytes(StateReady))

	t.mStatus = systray.AddMenuItem("Status: Ready (ws://localhost:"+fmt.Sprintf("%d", t.serverPort)+")", "Agent status")
	t.mStatus.Disable()

	systray.AddSeparator()

	mDashboard := systray.AddMenuItem("Open Dashboard", "Open local management dashboard in browser")
	mJobs := systray.AddMenuItem("View Recent Jobs", "View recent print job queue history")

	enabled, _ := autostart.IsEnabled()
	t.mAutoLaunch = systray.AddMenuItemCheckbox("Launch on OS Startup", "Toggle automatic boot startup", enabled)

	systray.AddSeparator()
	t.mUpdate = systray.AddMenuItem("Check for Updates", fmt.Sprintf("Current version: %s. Click to check for updates.", updater.Version))
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit PrintBridge", "Shutdown local agent")

	// Monitor queue events to update system tray status dynamically
	if t.q != nil {
		sub := t.q.Subscribe()
		go func() {
			for job := range sub {
				if job.Status == queue.StatusFailed {
					t.UpdateState(StateError, fmt.Sprintf("Job %s failed", job.ID[:8]))
				} else if job.Status == queue.StatusSending {
					t.UpdateState(StatePrinting, "Printing job...")
				} else if job.Status == queue.StatusSuccess {
					t.UpdateState(StateReady, "Ready")
				}
			}
		}()
	}

	go func() {
		for {
			select {
			case <-mDashboard.ClickedCh:
				openURL(fmt.Sprintf("http://localhost:%d/dashboard", t.serverPort))
			case <-mJobs.ClickedCh:
				openURL(fmt.Sprintf("http://localhost:%d/dashboard#jobs", t.serverPort))
			case <-t.mAutoLaunch.ClickedCh:
				if t.mAutoLaunch.Checked() {
					if err := autostart.Disable(); err == nil {
						t.mAutoLaunch.Uncheck()
						log.Println("[Tray] Disabled OS autostart")
					}
				} else {
					if err := autostart.Enable(""); err == nil {
						t.mAutoLaunch.Check()
						log.Println("[Tray] Enabled OS autostart")
					}
				}
			case <-t.mUpdate.ClickedCh:
				go func() {
					t.mUpdate.SetTitle("Checking for updates...")
					t.mUpdate.Disable()
					vers, avail, err := t.updater.CheckForUpdate()
					if err != nil {
						t.mUpdate.SetTitle(fmt.Sprintf("Update check failed: %v", err))
						log.Printf("[Tray] Update check failed: %v", err)
						time.Sleep(5 * time.Second)
						t.mUpdate.SetTitle("Check for Updates")
						t.mUpdate.Enable()
						return
					}
					if avail {
						log.Printf("[Tray] Update available: %s", vers)
						t.mUpdate.SetTitle(fmt.Sprintf("Update %s available - click to apply", vers))
						// Apply on next click
						select {
						case <-t.mUpdate.ClickedCh:
							t.mUpdate.SetTitle(fmt.Sprintf("Applying update %s...", vers))
							t.mUpdate.Disable()
							if err := t.updater.ApplyAvailableUpdate(); err != nil {
								t.mUpdate.SetTitle(fmt.Sprintf("Update failed: %v", err))
								log.Printf("[Tray] Update apply failed: %v", err)
							}
						case <-time.After(30 * time.Second):
							t.mUpdate.SetTitle("Check for Updates")
							t.mUpdate.Enable()
						}
					} else {
						t.mUpdate.SetTitle(fmt.Sprintf("Up to date (%s)", updater.Version))
						time.Sleep(3 * time.Second)
						t.mUpdate.SetTitle("Check for Updates")
						t.mUpdate.Enable()
					}
				}()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func (t *Tray) onExit() {
	log.Println("[Tray] Exiting system tray loop")
	if t.onQuit != nil {
		t.onQuit()
	}
}

func (t *Tray) UpdateState(state State, msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.currentState = state
	systray.SetIcon(getIconBytes(state))

	if t.mStatus != nil {
		switch state {
		case StateReady:
			t.mStatus.SetTitle(fmt.Sprintf("Status: Ready (ws://localhost:%d)", t.serverPort))
		case StatePrinting:
			t.mStatus.SetTitle("Status: Printing job...")
		case StateError:
			t.mStatus.SetTitle(fmt.Sprintf("Status: Error - %s", msg))
		case StateOffline:
			t.mStatus.SetTitle("Status: Printers Offline")
		}
	}
}

func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	_ = cmd.Start()
}

func getIconBytes(state State) []byte {
	// 1x1 transparent/colored ICO or PNG pixel bytes for tray icon fallback
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x10,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0xF3, 0xFF, 0x61, 0x00, 0x00, 0x00,
		0x19, 0x49, 0x44, 0x41, 0x54, 0x38, 0x11, 0x63, 0x60, 0x60, 0x60, 0xF8,
		0x0F, 0x00, 0x01, 0x04, 0x01, 0x00, 0x1C, 0xBD, 0xE7, 0x6E, 0x00, 0x00,
		0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}
}
