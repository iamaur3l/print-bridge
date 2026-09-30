package printer

import (
	"bytes"
	"strings"
	"testing"
)

func TestCapabilitiesDefaults(t *testing.T) {
	if got := (Capabilities{}).WithDefaults().MaxWidth; got != Width80mm {
		t.Errorf("expected a zero profile to default to %d columns, got %d", Width80mm, got)
	}

	if got := (Capabilities{MaxWidth: Width58mm}).WithDefaults().MaxWidth; got != Width58mm {
		t.Errorf("an explicit width must be preserved, got %d", got)
	}

	defaults := DefaultCapabilities()
	if defaults.MaxWidth != Width80mm || !defaults.SupportsCut || !defaults.SupportsCashDrawer {
		t.Errorf("unexpected default profile: %+v", defaults)
	}
}

func TestParseTestSlip(t *testing.T) {
	cases := map[string]TestSlip{
		"kitchen":  TestSlipKitchen,
		"KITCHEN":  TestSlipKitchen,
		" kitchen": TestSlipKitchen,
		"receipt":  TestSlipReceipt,
		"":         TestSlipReceipt,
		"bogus":    TestSlipReceipt,
		"text":     TestSlipText,
	}

	for input, want := range cases {
		if got := ParseTestSlip(input); got != want {
			t.Errorf("ParseTestSlip(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBuildTestSlipReceipt(t *testing.T) {
	caps := Capabilities{MaxWidth: Width58mm, SupportsBold: true, SupportsCut: true}
	payload := BuildTestSlip(TestSlipReceipt, "kitchen", "XP-80C", caps)

	if !bytes.HasPrefix(payload, escInit) {
		t.Errorf("expected the slip to start by initialising the printer, got % X", payload[:4])
	}
	if !bytes.Contains(payload, []byte("XP-80C (kitchen)")) {
		t.Error("expected the slip to name the printer and the role it serves")
	}
	if !bytes.Contains(payload, []byte("PRINTBRIDGE")) {
		t.Error("expected the slip to be titled")
	}

	// The divider honours the configured width, which is what keeps a test slip from
	// wrapping unpredictably on a 58mm device.
	if !bytes.Contains(payload, []byte(strings.Repeat("-", Width58mm))) {
		t.Error("expected a divider at the configured width")
	}
	if bytes.Contains(payload, []byte(strings.Repeat("-", Width58mm+1))) {
		t.Error("divider exceeded the configured width")
	}

	// Cut is only emitted when the printer can do it, and the full cut is used when
	// partial cutting is not supported.
	if !bytes.Contains(payload, escCutFull) {
		t.Error("expected a full cut for a printer without partial-cut support")
	}
}

func TestBuildTestSlipRespectsMissingCapabilities(t *testing.T) {
	payload := BuildTestSlip(TestSlipReceipt, "", "Plain Printer", Capabilities{MaxWidth: Width80mm})

	if bytes.Contains(payload, escCutFull) || bytes.Contains(payload, escCutPartial) {
		t.Error("a printer that cannot cut must not be sent a cut command")
	}
	if bytes.Contains(payload, escBoldOn) {
		t.Error("a printer without bold support must not be sent bold sequences")
	}
	if !bytes.Contains(payload, []byte("Plain Printer")) {
		t.Error("expected the printer name even without a role")
	}
}

func TestBuildTestSlipKitchenUsesWideBold(t *testing.T) {
	caps := Capabilities{MaxWidth: Width80mm, SupportsBold: true, SupportsCut: true, SupportsPartialCut: true}
	payload := BuildTestSlip(TestSlipKitchen, "kitchen", "Kitchen Printer", caps)

	if !bytes.Contains(payload, []byte("KITCHEN ORDER")) {
		t.Error("expected the kitchen slip to be titled for the station")
	}
	if !bytes.Contains(payload, escDoubleOn) {
		t.Error("expected the kitchen slip to use double-width text")
	}
	if !bytes.Contains(payload, escCutPartial) {
		t.Error("expected a partial cut when the printer supports it")
	}
	if bytes.Contains(payload, []byte("Thank you")) {
		t.Error("a kitchen ticket is not a customer receipt")
	}
}

func TestBuildTestSlipText(t *testing.T) {
	payload := BuildTestSlip(TestSlipText, "", "Troubleshooting Printer", DefaultCapabilities())

	if !bytes.Contains(payload, []byte("Test print for Troubleshooting Printer")) {
		t.Error("expected the plain slip to name the printer")
	}
	if bytes.Contains(payload, []byte("TOTAL")) {
		t.Error("the text slip should not pretend to be a receipt")
	}
}
