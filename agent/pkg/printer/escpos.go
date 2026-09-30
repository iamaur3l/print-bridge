package printer

import (
	"fmt"
	"strings"
	"time"
)

// Width58mm and Width80mm are the character widths of the two common thermal paper
// sizes at the default font.
const (
	Width58mm = 32
	Width80mm = 48
)

// Capabilities describes what a physical printer can do when printing for a role.
// The zero value is completed by WithDefaults.
type Capabilities struct {
	MaxWidth           int  `json:"max_width"`
	SupportsBold       bool `json:"supports_bold"`
	SupportsUnderline  bool `json:"supports_underline"`
	SupportsBarcode    bool `json:"supports_barcode"`
	SupportsQRCode     bool `json:"supports_qr_code"`
	SupportsImage      bool `json:"supports_image"`
	SupportsCut        bool `json:"supports_cut"`
	SupportsPartialCut bool `json:"supports_partial_cut"`
	SupportsCashDrawer bool `json:"supports_cash_drawer"`
	Codepage           int  `json:"codepage"`
}

// DefaultCapabilities is the profile of a typical 80mm ESC/POS receipt printer.
func DefaultCapabilities() Capabilities {
	return Capabilities{
		MaxWidth:           Width80mm,
		SupportsBold:       true,
		SupportsUnderline:  true,
		SupportsCut:        true,
		SupportsPartialCut: true,
		SupportsCashDrawer: true,
	}
}

// WithDefaults fills in a usable character width when a profile does not set one.
func (c Capabilities) WithDefaults() Capabilities {
	if c.MaxWidth <= 0 {
		c.MaxWidth = Width80mm
	}
	return c
}

// ESC/POS control sequences used by the built-in test slips.
var (
	escInit        = []byte{0x1B, 0x40}
	escAlignLeft   = []byte{0x1B, 0x61, 0x00}
	escAlignCenter = []byte{0x1B, 0x61, 0x01}
	escBoldOn      = []byte{0x1B, 0x45, 0x01}
	escBoldOff     = []byte{0x1B, 0x45, 0x00}
	escDoubleOn    = []byte{0x1D, 0x21, 0x11}
	escDoubleOff   = []byte{0x1D, 0x21, 0x00}
	escCutFull     = []byte{0x1D, 0x56, 0x41, 0x00}
	escCutPartial  = []byte{0x1D, 0x56, 0x42, 0x00}
)

// TestSlip names a built-in test print.
type TestSlip string

const (
	// TestSlipReceipt is a customer-style receipt with a cut.
	TestSlipReceipt TestSlip = "receipt"
	// TestSlipKitchen is a bold, wide kitchen ticket.
	TestSlipKitchen TestSlip = "kitchen"
	// TestSlipText is a minimal plain-text slip for troubleshooting.
	TestSlipText TestSlip = "text"
)

// ParseTestSlip maps a request string to a slip, defaulting to a receipt.
func ParseTestSlip(value string) TestSlip {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(TestSlipKitchen):
		return TestSlipKitchen
	case string(TestSlipText):
		return TestSlipText
	default:
		return TestSlipReceipt
	}
}

// BuildTestSlip builds an ESC/POS test print for a role target, honouring the
// configured character width and only emitting sequences the printer claims to
// support. It is what the `test_print` request sends, and what an installer uses to
// prove a role is wired to the right device.
func BuildTestSlip(slip TestSlip, role, printerName string, caps Capabilities) []byte {
	caps = caps.WithDefaults()
	width := caps.MaxWidth
	if width < 16 {
		width = Width80mm
	}

	title := "PRINTBRIDGE"
	if slip == TestSlipKitchen {
		title = "KITCHEN ORDER"
	}

	buf := make([]byte, 0, 512)
	buf = append(buf, escInit...)

	centre := func(s string) {
		buf = append(buf, escAlignCenter...)
		buf = append(buf, []byte(s)...)
		buf = append(buf, '\n')
	}
	lines := func(s string) {
		buf = append(buf, escAlignLeft...)
		for _, line := range strings.Split(s, "\n") {
			buf = append(buf, []byte(fitWidth(line, width))...)
			buf = append(buf, '\n')
		}
	}

	if caps.SupportsBold {
		buf = append(buf, escBoldOn...)
	}
	if slip == TestSlipKitchen {
		buf = append(buf, escDoubleOn...)
	}
	centre(title)
	if slip == TestSlipKitchen {
		buf = append(buf, escDoubleOff...)
	}
	if caps.SupportsBold {
		buf = append(buf, escBoldOff...)
	}

	target := printerName
	if role != "" {
		target = fmt.Sprintf("%s (%s)", printerName, role)
	}
	centre(target)
	centre(time.Now().Format("2006-01-02 15:04:05"))
	lines(strings.Repeat("-", width))

	if slip == TestSlipText {
		lines(fmt.Sprintf("Test print for %s.", target))
		lines("If you can read this, the queue, transport and device all work.")
	} else {
		lines("2 x Test Item A")
		lines(fmt.Sprintf("%*s", width, "12.00"))
		lines("1 x Test Item B")
		lines(fmt.Sprintf("%*s", width, " 6.50"))
		lines(strings.Repeat("-", width))
		lines(fmt.Sprintf("%-12s%*s", "TOTAL", width-12, "18.50"))
		if slip == TestSlipReceipt {
			lines("")
			centre("Thank you")
		}
	}

	lines("")
	lines("")

	// Only cut when the printer claims the capability.
	if caps.SupportsCut {
		if caps.SupportsPartialCut {
			buf = append(buf, escCutPartial...)
		} else {
			buf = append(buf, escCutFull...)
		}
	}

	return buf
}

// fitWidth truncates a line to the printer's character width so a test slip cannot
// wrap unpredictably on a narrow device.
func fitWidth(line string, width int) string {
	if len(line) <= width {
		return line
	}
	return line[:width]
}
