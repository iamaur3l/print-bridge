package printer

import (
	"errors"
	"fmt"
)

// PrinterInfo represents system printer metadata.
type PrinterInfo struct {
	Name       string `json:"name"`
	DriverName string `json:"driver_name"`
	PortName   string `json:"port_name"`
	IsDefault  bool   `json:"is_default"`
	IsOnline   bool   `json:"is_online"`
	// State is the corroborated verdict: "online", "offline" or "unknown".
	// "unknown" exists so a stale spooler flag never libels a working printer.
	State string `json:"state,omitempty"`
	// StatusDescription is the human-readable label for State.
	StatusDescription string `json:"status_description,omitempty"`
	// StatusDetail explains how the verdict was reached.
	StatusDetail string `json:"status_detail,omitempty"`
	// StaleWorkOffline reports a spooler offline flag that was contradicted.
	StaleWorkOffline bool `json:"stale_work_offline,omitempty"`
	// Type is "local" (printed through the OS spooler) or "network" (written
	// directly to the printer's TCP RAW port).
	Type string `json:"type,omitempty"`
	// NetworkAddress is "host:port" for network printers.
	NetworkAddress string `json:"network_address,omitempty"`
}

// Common errors
var (
	ErrPrinterNotFound = errors.New("printer not found")
	ErrPrintFailed     = errors.New("failed to send print job to spooler")
	ErrEmptyPayload    = errors.New("payload cannot be empty")
)

// VerifyPayload ensures payload data is valid before sending.
func VerifyPayload(data []byte) error {
	if len(data) == 0 {
		return ErrEmptyPayload
	}
	return nil
}

// FormatJobName creates a safe, identifiable print job name.
func FormatJobName(jobName string) string {
	if jobName == "" {
		return "PrintBridge Raw Print Job"
	}
	return fmt.Sprintf("PrintBridge - %s", jobName)
}
