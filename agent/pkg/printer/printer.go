package printer

import (
	"errors"
	"fmt"
)

// PrinterInfo represents system printer metadata.
type PrinterInfo struct {
	Name              string `json:"name"`
	DriverName        string `json:"driver_name"`
	PortName          string `json:"port_name"`
	IsDefault         bool   `json:"is_default"`
	IsOnline          bool   `json:"is_online"`
	StatusDescription string `json:"status_description,omitempty"`
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
