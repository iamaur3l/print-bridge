package devices

import (
	"fmt"

	"github.com/printbridge/printbridge/agent/pkg/printer"
)

// DrawerPin defines RJ11/RJ12 drawer kick pin configuration.
type DrawerPin int

const (
	Pin2 DrawerPin = 2
	Pin5 DrawerPin = 5
)

// GetDrawerPulseBytes returns ESC/POS pulse sequence for requested drawer pin or printer brand.
func GetDrawerPulseBytes(pin DrawerPin, brand string) []byte {
	if brand == "star" {
		// Star Micronics BEL character pulse
		return []byte{0x07}
	}

	if pin == Pin5 {
		// ESC p 1 25 250 (Pin 5 pulse)
		return []byte{0x1B, 0x70, 0x01, 0x19, 0xFA}
	}

	// ESC p 0 25 250 (Default Pin 2 pulse for Epson / Citizen / Bixolon)
	return []byte{0x1B, 0x70, 0x00, 0x19, 0xFA}
}

// OpenDrawer sends a raw drawer-kick byte pulse sequence to a receipt printer.
func OpenDrawer(printerName string, pin DrawerPin, brand string) error {
	if printerName == "" {
		return fmt.Errorf("printerName is required to kick cash drawer")
	}

	pulse := GetDrawerPulseBytes(pin, brand)
	return printer.PrintRaw(printerName, pulse, "Cash Drawer Kick")
}
