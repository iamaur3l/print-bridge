package devices

import (
	"bytes"
	"testing"
)

func TestDrawerPulseBytes(t *testing.T) {
	pin2Pulse := GetDrawerPulseBytes(Pin2, "epson")
	expectedPin2 := []byte{0x1B, 0x70, 0x00, 0x19, 0xFA}
	if !bytes.Equal(pin2Pulse, expectedPin2) {
		t.Errorf("unexpected Pin 2 pulse: %v", pin2Pulse)
	}

	pin5Pulse := GetDrawerPulseBytes(Pin5, "epson")
	expectedPin5 := []byte{0x1B, 0x70, 0x01, 0x19, 0xFA}
	if !bytes.Equal(pin5Pulse, expectedPin5) {
		t.Errorf("unexpected Pin 5 pulse: %v", pin5Pulse)
	}

	starPulse := GetDrawerPulseBytes(Pin2, "star")
	expectedStar := []byte{0x07}
	if !bytes.Equal(starPulse, expectedStar) {
		t.Errorf("unexpected Star pulse: %v", starPulse)
	}
}

func TestSerialPortsListing(t *testing.T) {
	ports, err := ListSerialPorts()
	if err != nil {
		t.Fatalf("ListSerialPorts failed: %v", err)
	}
	t.Logf("Discovered %d serial ports", len(ports))
}

func TestHIDDevicesListing(t *testing.T) {
	devs, err := ListHIDDevices()
	if err != nil {
		t.Fatalf("ListHIDDevices failed: %v", err)
	}
	t.Logf("Discovered %d HID devices", len(devs))
}
