package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const AppName = "PrintBridgeAgent"

// Enable configures the OS to automatically launch PrintBridge Agent on boot/login.
func Enable(execPath string) error {
	if execPath == "" {
		var err error
		execPath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("failed to get executable path: %w", err)
		}
	}
	execPath, _ = filepath.Abs(execPath)

	switch runtime.GOOS {
	case "windows":
		return enableWindows(execPath)
	case "darwin":
		return enableDarwin(execPath)
	case "linux":
		return enableLinux(execPath)
	default:
		return fmt.Errorf("autostart unsupported on OS: %s", runtime.GOOS)
	}
}

// Disable removes the OS autostart entry for PrintBridge Agent.
func Disable() error {
	switch runtime.GOOS {
	case "windows":
		return disableWindows()
	case "darwin":
		return disableDarwin()
	case "linux":
		return disableLinux()
	default:
		return fmt.Errorf("autostart unsupported on OS: %s", runtime.GOOS)
	}
}

// IsEnabled checks if autostart is currently enabled.
func IsEnabled() (bool, error) {
	switch runtime.GOOS {
	case "windows":
		return isEnabledWindows()
	case "darwin":
		return isEnabledDarwin()
	case "linux":
		return isEnabledLinux()
	default:
		return false, nil
	}
}

func enableDarwin(execPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	plistPath := filepath.Join(dir, "com.printbridge.agent.plist")

	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.printbridge.agent</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>`, execPath)

	return os.WriteFile(plistPath, []byte(content), 0644)
}

func disableDarwin() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.printbridge.agent.plist")
	_ = os.Remove(plistPath)
	return nil
}

func isEnabledDarwin() (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.printbridge.agent.plist")
	_, err = os.Stat(plistPath)
	return err == nil, nil
}

func enableLinux(execPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "autostart")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	desktopPath := filepath.Join(dir, "printbridge.desktop")

	content := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=PrintBridge Agent
Exec=%s
Hidden=false
NoDisplay=false
X-GNOME-Autostart-enabled=true
`, execPath)

	return os.WriteFile(desktopPath, []byte(content), 0644)
}

func disableLinux() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	desktopPath := filepath.Join(home, ".config", "autostart", "printbridge.desktop")
	_ = os.Remove(desktopPath)
	return nil
}

func isEnabledLinux() (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	desktopPath := filepath.Join(home, ".config", "autostart", "printbridge.desktop")
	_, err = os.Stat(desktopPath)
	return err == nil, nil
}
