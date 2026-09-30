//go:build windows

package printer

import (
	"fmt"
	"sync"
	"syscall"
	"time"
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

	PRINTER_STATUS_PAUSED            = 0x00000001
	PRINTER_STATUS_ERROR             = 0x00000002
	PRINTER_STATUS_PENDING_DELETION  = 0x00000004
	PRINTER_STATUS_PAPER_JAM         = 0x00000008
	PRINTER_STATUS_PAPER_OUT         = 0x00000010
	PRINTER_STATUS_MANUAL_FEED       = 0x00000020
	PRINTER_STATUS_PAPER_PROBLEM     = 0x00000040
	PRINTER_STATUS_OFFLINE           = 0x00000080
	PRINTER_STATUS_OUTPUT_BIN_FULL   = 0x00000800
	PRINTER_STATUS_NOT_AVAILABLE     = 0x00001000
	PRINTER_STATUS_NO_TONER          = 0x00040000
	PRINTER_STATUS_USER_INTERVENTION = 0x00100000
	PRINTER_STATUS_DOOR_OPEN         = 0x00400000
	PRINTER_STATUS_SERVER_UNKNOWN    = 0x00800000
)

// statusEvidence converts raw PRINTER_INFO_2 fields into corroboration evidence.
func statusEvidence(info *PRINTER_INFO_2W, printerName string) StatusEvidence {
	return StatusEvidence{
		WorkOfflineAttr:  info.Attributes&PRINTER_ATTRIBUTE_WORK_OFFLINE != 0,
		OfflineBit:       info.Status&PRINTER_STATUS_OFFLINE != 0,
		Paused:           info.Status&PRINTER_STATUS_PAUSED != 0,
		ErrorBit:         info.Status&PRINTER_STATUS_ERROR != 0,
		PaperJam:         info.Status&PRINTER_STATUS_PAPER_JAM != 0,
		PaperOut:         info.Status&PRINTER_STATUS_PAPER_OUT != 0,
		NoToner:          info.Status&PRINTER_STATUS_NO_TONER != 0,
		UserIntervention: info.Status&PRINTER_STATUS_USER_INTERVENTION != 0,
		DoorOpen:         info.Status&PRINTER_STATUS_DOOR_OPEN != 0,
		// An unreachable print server is real corroboration, not a stale flag.
		NotAvailable: info.Status&(PRINTER_STATUS_NOT_AVAILABLE|PRINTER_STATUS_SERVER_UNKNOWN) != 0,
		LastWriteOK:  LastSuccessfulWrite(printerName),
	}
}

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

	type queueEntry struct {
		info      *PRINTER_INFO_2W
		name      string
		driver    string
		port      string
		target    NetworkTarget
		isNetwork bool
	}

	infoSize := unsafe.Sizeof(PRINTER_INFO_2W{})
	entries := make([]queueEntry, 0, returned)

	for i := uint32(0); i < returned; i++ {
		info := (*PRINTER_INFO_2W)(unsafe.Pointer(&buffer[uintptr(i)*infoSize]))

		entry := queueEntry{
			info: info,
			name: syscall.UTF16ToString((*[1 << 16]uint16)(unsafe.Pointer(info.PPrinterName))[:]),
		}
		if info.PDriverName != nil {
			entry.driver = syscall.UTF16ToString((*[1 << 16]uint16)(unsafe.Pointer(info.PDriverName))[:])
		}
		if info.PPortName != nil {
			entry.port = syscall.UTF16ToString((*[1 << 16]uint16)(unsafe.Pointer(info.PPortName))[:])
		}

		// A queue on a TCP port can be probed and, when printing, written to
		// directly — which takes the spooler and its stale offline flags out of the
		// path entirely.
		if target, ok := ParseNetworkPort(entry.port); ok {
			entry.target, entry.isNetwork = target, true
		}

		entries = append(entries, entry)
	}

	// Probe network printers in parallel with a short deadline. The result is cached
	// by the shared snapshot, so this cost is paid at most once per refresh.
	reachable := make(map[string]*bool, len(entries))
	var probeMu sync.Mutex
	var probes sync.WaitGroup

	for _, e := range entries {
		if !e.isNetwork {
			continue
		}

		probes.Add(1)
		go func(e queueEntry) {
			defer probes.Done()

			ok := ProbeNetworkTarget(e.target, DefaultProbeTimeout) == nil

			probeMu.Lock()
			reachable[e.name] = &ok
			probeMu.Unlock()
		}(e)
	}
	probes.Wait()

	printers := make([]PrinterInfo, 0, len(entries))

	for _, e := range entries {
		// Never trust a single signal: the WorkOffline flag is routinely stale on
		// USB thermal printers, so an offline verdict needs a real fault, a
		// physically absent port, an unreachable transport, or a failed write — and
		// a completed write or a successful probe overrides everything the spooler
		// says.
		evidence := statusEvidence(e.info, e.name)
		evidence.Reachable = reachable[e.name]

		verdict := EvaluateStatus(evidence, time.Now())

		pi := PrinterInfo{
			Name:              e.name,
			DriverName:        e.driver,
			PortName:          e.port,
			IsDefault:         e.name == defaultPrinterName,
			IsOnline:          verdict.State == StateOnline,
			State:             string(verdict.State),
			StatusDescription: verdict.Description,
			StatusDetail:      verdict.Detail,
			StaleWorkOffline:  verdict.StaleFlags,
			Type:              "local",
		}
		if e.isNetwork {
			pi.Type = "network"
			pi.NetworkAddress = e.target.Address()
		}

		printers = append(printers, pi)
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

	// The write completed, so this printer is reachable no matter what the
	// spooler's status flags claim about it.
	RecordSuccessfulWrite(printerName)

	return nil
}
