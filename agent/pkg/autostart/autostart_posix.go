//go:build !windows

package autostart

func enableWindows(execPath string) error {
	return nil
}

func disableWindows() error {
	return nil
}

func isEnabledWindows() (bool, error) {
	return false, nil
}
