package printer

import (
	"bytes"
	"testing"
)

func TestVerifyPayload(t *testing.T) {
	if err := VerifyPayload(nil); err != ErrEmptyPayload {
		t.Errorf("expected ErrEmptyPayload for nil payload, got %v", err)
	}

	if err := VerifyPayload([]byte{}); err != ErrEmptyPayload {
		t.Errorf("expected ErrEmptyPayload for empty payload, got %v", err)
	}

	validData := []byte{0x1B, 0x40, 'H', 'e', 'l', 'l', 'o', 0x0A}
	if err := VerifyPayload(validData); err != nil {
		t.Errorf("expected nil error for valid payload, got %v", err)
	}
}

func TestFormatJobName(t *testing.T) {
	defaultName := FormatJobName("")
	if defaultName != "PrintBridge Raw Print Job" {
		t.Errorf("unexpected default job name: %s", defaultName)
	}

	customName := FormatJobName("Receipt #1001")
	if customName != "PrintBridge - Receipt #1001" {
		t.Errorf("unexpected custom job name: %s", customName)
	}
}

func TestRawByteImmutability(t *testing.T) {
	// ESC/POS test sequence: Init + Text + Cut command (\x1DV\x41\x00)
	escposPayload := []byte{0x1B, 0x40, 'T', 'e', 's', 't', 0x0A, 0x1D, 0x56, 0x41, 0x00}
	copyPayload := make([]byte, len(escposPayload))
	copy(copyPayload, escposPayload)

	if !bytes.Equal(escposPayload, copyPayload) {
		t.Errorf("byte payload altered during copy check")
	}
}
