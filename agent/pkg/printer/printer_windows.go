//go:build windows

package printer

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	modWinspool          = syscall.NewLazyDLL("winspool.drv")
	procEnumPrintersW    = modWinspool.NewProc("EnumPrintersW")
	procGetDefaultPrinter = modWinspool.NewProc("GetDefaultPrinterW")
	procOpenPrinterW     = modWinspool.NewProc("OpenPrinterW")
	procStartDocPrinterW = modWinspool.NewProc("StartDocPrinterW")
	procStartPagePrinter = modWinspool.NewProc("StartPagePrinterW")
	procWritePrinter     = modWinspool.NewProc("WritePrinter")
	procEndPagePrinter   = modWinspool.NewProc("EndPagePrinterW")
	procEndDocPrinter     = modWinspool.NewProc("EndDocPrinterW")
	procClosePrinter     = modWinspool.NewProc("ClosePrinter")
)

const (
	PRINTER_ENUM_LOCAL       = 0x00000002
	PRINTER_ENUM_CONNECTIONS = 0x00000004

	PRINTER_ATTRIBUTE_WORK_OFFLINE = 0x00000400
	PRINTER_STATUS_OFFLINE         = 0x00000080
	PRINTER_STATUS_ERROR           = 0x00000002
	PRINTER_STATUS_PAPER_JAM       = 0x00000008
	PRINTER_STATUS_PAPER_OUT       = 0x00000010
)

type PRINTER_INFO_2W struct {
	PServerName         *uint16
	PPrinterName        *uint16
	PShareName          *uint16
	PPortName           *uint16
	PDriverName         *uint16
	PComment            *uint16
	PLocation           *uint16
	PDevMode            uintptr
	PSepFile            *uint16
	PPrintProcessor     *uint16
	PDatatype           *uint16
	PParameters         *uint16
	PSecurityDescriptor uintptr
	Attributes          uint32
	Priority            uint32
	DefaultPriority     uint32
	StartTime           uint32
	UntilTime           uint32
	Status              uint32
	CJobs               uint32
	AveragePPM          uint32
}

type DOC_INFO_1W struct {
	PDocName    *uint16
	POutputFile *uint16
	PDatatype   *uint16
}

// GetDefaultPrinter returns the default system printer name on Windows.
func GetDefaultPrinter() (string, error) {
	var bufSize uint32
	procGetDefaultPrinter.Call(0, uintptr(unsafe.Pointer(&bufSize)))
	if bufSize == 0 {
		return "", fmt.Errorf("failed to get default printer name size")
	}

	buf := make([]uint16, bufSize)
	ret, _, err := procGetDefaultPrinter.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bufSize)))
	if ret == 0 {
		return "", fmt.Errorf("GetDefaultPrinterW error: %w", err)
	}

	return syscall.UTF16ToString(buf), nil
}

// ListPrinters enumerates all local and network connected printers.
func ListPrinters() ([]PrinterInfo, error) {
	defaultPrinterName, _ := GetDefaultPrinter()

	flags := uint32(PRINTER_ENUM_LOCAL | PRINTER_ENUM_CONNECTIONS)
	level := uint32(2)

	var needed, returned uint32
	procEnumPrintersW.Call(
		uintptr(flags),
		0,
		uintptr(level),
		0,
		0,
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)

	if needed == 0 {
		return []PrinterInfo{}, nil
	}

	buffer := make([]byte, needed)
	ret, _, err := procEnumPrintersW.Call(
		uintptr(flags),
		0,
		uintptr(level),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)

	if ret == 0 {
		return nil, fmt.Errorf("EnumPrintersW failed: %w", err)
	}

	printers := make([]PrinterInfo, 0, returned)
	infoSize := unsafe.Sizeof(PRINTER_INFO_2W{})

	for i := uint32(0); i < returned; i++ {
		info := (*PRINTER_INFO_2W)(unsafe.Pointer(&buffer[uintptr(i)*infoSize]))

		name := syscall.UTF16ToString((*[1 << 16]uint16)(unsafe.Pointer(info.PPrinterName))[:])
		driver := ""
		if info.PDriverName != nil {
			driver = syscall.UTF16ToString((*[1 << 16]uint16)(unsafe.Pointer(info.PDriverName))[:])
		}
		port := ""
		if info.PPortName != nil {
			port = syscall.UTF16ToString((*[1 << 16]uint16)(unsafe.Pointer(info.PPortName))[:])
		}

		isOffline := (info.Attributes&PRINTER_ATTRIBUTE_WORK_OFFLINE != 0) || (info.Status&PRINTER_STATUS_OFFLINE != 0)
		statusDesc := "Ready"
		if isOffline {
			statusDesc = "Offline"
		} else if info.Status&PRINTER_STATUS_ERROR != 0 {
			statusDesc = "Error"
		} else if info.Status&PRINTER_STATUS_PAPER_JAM != 0 {
			statusDesc = "Paper Jam"
		} else if info.Status&PRINTER_STATUS_PAPER_OUT != 0 {
			statusDesc = "Paper Out"
		}

		printers = append(printers, PrinterInfo{
			Name:              name,
			DriverName:        driver,
			PortName:          port,
			IsDefault:         name == defaultPrinterName,
			IsOnline:          !isOffline,
			StatusDescription: statusDesc,
		})
	}

	return printers, nil
}

// PrintRaw sends raw byte sequence directly to the named Windows printer spooler.
func PrintRaw(printerName string, data []byte, jobName string) error {
	if err := VerifyPayload(data); err != nil {
		return err
	}

	printerNamePtr, err := syscall.UTF16PtrFromString(printerName)
	if err != nil {
		return fmt.Errorf("invalid printer name string: %w", err)
	}

	var hPrinter syscall.Handle
	ret, _, err := procOpenPrinterW.Call(
		uintptr(unsafe.Pointer(printerNamePtr)),
		uintptr(unsafe.Pointer(&hPrinter)),
		0,
	)
	if ret == 0 {
		return fmt.Errorf("OpenPrinterW failed for %q: %w", printerName, err)
	}
	defer procClosePrinter.Call(uintptr(hPrinter))

	docNamePtr, _ := syscall.UTF16PtrFromString(FormatJobName(jobName))
	datatypePtr, _ := syscall.UTF16PtrFromString("RAW")

	docInfo := DOC_INFO_1W{
		PDocName:    docNamePtr,
		POutputFile: nil,
		PDatatype:   datatypePtr,
	}

	jobID, _, err := procStartDocPrinterW.Call(
		uintptr(hPrinter),
		1,
		uintptr(unsafe.Pointer(&docInfo)),
	)
	if jobID == 0 {
		return fmt.Errorf("StartDocPrinterW failed: %w", err)
	}
	defer procEndDocPrinter.Call(uintptr(hPrinter))

	ret, _, err = procStartPagePrinter.Call(uintptr(hPrinter))
	if ret == 0 {
		return fmt.Errorf("StartPagePrinterW failed: %w", err)
	}
	defer procEndPagePrinter.Call(uintptr(hPrinter))

	var written uint32
	ret, _, err = procWritePrinter.Call(
		uintptr(hPrinter),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(unsafe.Pointer(&written)),
	)
	if ret == 0 || written != uint32(len(data)) {
		return fmt.Errorf("WritePrinter failed (wrote %d/%d bytes): %w", written, len(data), err)
	}

	return nil
}
