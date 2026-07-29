//go:build !windows

package printer

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// ListPrinters enumerates installed printers on POSIX systems via lpstat.
func ListPrinters() ([]PrinterInfo, error) {
	cmd := exec.Command("lpstat", "-e")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run lpstat -e: %w", err)
	}

	defaultCmd := exec.Command("lpstat", "-d")
	defaultOut, _ := defaultCmd.Output()
	defaultPrinter := ""
	if parts := strings.Split(string(defaultOut), ": "); len(parts) > 1 {
		defaultPrinter = strings.TrimSpace(parts[1])
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	printers := make([]PrinterInfo, 0, len(lines))

	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		printers = append(printers, PrinterInfo{
			Name:              name,
			DriverName:        "CUPS Printer Driver",
			PortName:          "CUPS",
			IsDefault:         name == defaultPrinter,
			IsOnline:          true,
			StatusDescription: "Ready",
		})
	}

	return printers, nil
}

// PrintRaw sends raw byte stream to printer via CUPS lpr.
func PrintRaw(printerName string, data []byte, jobName string) error {
	if err := VerifyPayload(data); err != nil {
		return err
	}

	args := []string{"-P", printerName, "-o", "raw"}
	if jobName != "" {
		args = append(args, "-J", FormatJobName(jobName))
	}

	cmd := exec.Command("lpr", args...)
	cmd.Stdin = bytes.NewReader(data)

	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("lpr raw print error: %w (stderr: %s)", err, strings.TrimSpace(errBuf.String()))
	}

	return nil
}
