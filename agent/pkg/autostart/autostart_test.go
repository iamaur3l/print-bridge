package autostart

import (
	"testing"
)

func TestAutostartToggle(t *testing.T) {
	// Verify autostart status query does not panic
	enabled, err := IsEnabled()
	if err != nil {
		t.Fatalf("IsEnabled failed: %v", err)
	}
	t.Logf("Current autostart enabled status: %t", enabled)
}
