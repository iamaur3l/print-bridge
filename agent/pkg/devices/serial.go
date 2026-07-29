package devices

import (
	"fmt"

	"go.bug.st/serial"
)

type SerialPortInfo struct {
	PortName string `json:"port_name"`
	IsUSB    bool   `json:"is_usb"`
}

// ListSerialPorts enumerates available COM / tty serial ports on the system.
func ListSerialPorts() ([]SerialPortInfo, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil, fmt.Errorf("failed to list serial ports: %w", err)
	}

	result := make([]SerialPortInfo, 0, len(ports))
	for _, p := range ports {
		result = append(result, SerialPortInfo{
			PortName: p,
			IsUSB:    false,
		})
	}
	return result, nil
}
