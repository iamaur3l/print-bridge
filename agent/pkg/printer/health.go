package printer

import (
	"fmt"
	"sync"
	"time"
)

// PrinterState is the corroborated health of a print queue.
type PrinterState string

const (
	StateOnline  PrinterState = "online"
	StateOffline PrinterState = "offline"

	// StateUnknown means the spooler reported something — almost always the
	// stale WorkOffline flag on a USB thermal printer — that no fault bit and no
	// physical absence corroborates. It is deliberately distinct from "offline"
	// so a working printer is never reported as broken.
	StateUnknown PrinterState = "unknown"
)

// WriteTrustWindow is how long a successful raw write keeps a printer "online"
// regardless of what the spooler claims. A completed write is ground truth.
const WriteTrustWindow = 2 * time.Minute

// StatusEvidence is the raw, per-OS evidence about a print queue.
// Windows populates it from PRINTER_INFO_2; POSIX leaves it empty because CUPS
// status reporting is trusted as-is.
type StatusEvidence struct {
	// WorkOfflineAttr is PRINTER_ATTRIBUTE_WORK_OFFLINE. Windows sets it when a
	// USB device disappears and frequently never clears it when the device
	// returns, so it must never decide a verdict on its own.
	WorkOfflineAttr bool
	// OfflineBit is PRINTER_STATUS_OFFLINE, which is stale in the same way.
	OfflineBit bool

	Paused           bool
	ErrorBit         bool
	PaperJam         bool
	PaperOut         bool
	NoToner          bool
	UserIntervention bool
	DoorOpen         bool
	NotAvailable     bool

	// PortAbsent means the port the queue is bound to does not physically exist
	// (for example a USB port with no device attached). This is real
	// corroboration that the printer is gone.
	PortAbsent bool

	// LastWriteOK is when a raw print last completed successfully.
	LastWriteOK time.Time

	// Reachable is the result of probing the printer's transport — a TCP connection
	// to the RAW port for network printers. Nil means the transport cannot be probed.
	// A reachable device is real corroboration, exactly like a completed write.
	Reachable *bool
}

// StatusVerdict is the result of evaluating status evidence.
type StatusVerdict struct {
	State       PrinterState
	Description string
	Detail      string
	// StaleFlags reports that the spooler's offline flags were contradicted.
	StaleFlags bool
}

// EvaluateStatus applies the corroboration rules, in priority order:
//
//  1. A hardware fault (jam, paper out, no toner, door open, user intervention,
//     printer error) means offline. A spooler write cannot disprove a physical
//     fault — on Windows WritePrinter succeeds into the queue even when the
//     device is out of paper.
//  2. A completed write inside WriteTrustWindow means online: it proves the
//     spooler path works right now, which overrides the offline flags.
//  3. A transport probe (TCP 9100 for network printers) decides online/offline
//     outright, because it is a direct observation of the device.
//  4. A paused queue, or a port that does not physically exist, means offline.
//  5. The offline family of flags on its own is *not* enough: report "unknown"
//     rather than libelling a working printer.
//  6. Anything unrecognised counts as ready, never as an error — cheap thermal
//     units routinely report status codes Windows cannot classify.
func EvaluateStatus(ev StatusEvidence, now time.Time) StatusVerdict {
	if fault := describeFault(ev); fault != "" {
		return StatusVerdict{
			State:       StateOffline,
			Description: fault,
			Detail:      "reported by the print spooler",
		}
	}

	if !ev.LastWriteOK.IsZero() && now.Sub(ev.LastWriteOK) <= WriteTrustWindow {
		return StatusVerdict{
			State:       StateOnline,
			Description: "Ready (write verified)",
			Detail: fmt.Sprintf("a raw print completed successfully %s ago, overriding spooler flags",
				now.Sub(ev.LastWriteOK).Round(time.Second)),
			StaleFlags: ev.WorkOfflineAttr || ev.OfflineBit,
		}
	}

	if ev.Reachable != nil {
		if !*ev.Reachable {
			return StatusVerdict{
				State:       StateOffline,
				Description: "Not reachable",
				Detail:      "the printer refused or ignored a connection on its RAW port",
			}
		}
		return StatusVerdict{
			State:       StateOnline,
			Description: "Ready (transport reachable)",
			Detail:      "a TCP connection to the printer's RAW port succeeded, overriding spooler flags",
			StaleFlags:  ev.WorkOfflineAttr || ev.OfflineBit,
		}
	}

	if ev.Paused {
		return StatusVerdict{
			State:       StateOffline,
			Description: "Paused",
			Detail:      "the print queue is paused and will not dispatch jobs",
		}
	}

	if ev.PortAbsent {
		return StatusVerdict{
			State:       StateOffline,
			Description: "Port not present",
			Detail:      "the printer's port does not exist on this machine",
		}
	}

	if ev.WorkOfflineAttr || ev.OfflineBit {
		return StatusVerdict{
			State:       StateUnknown,
			Description: "Status unverified (spooler reports offline)",
			Detail:      "no fault and no missing port corroborates it; a successful print overrides this",
			StaleFlags:  true,
		}
	}

	return StatusVerdict{State: StateOnline, Description: "Ready"}
}

func describeFault(ev StatusEvidence) string {
	switch {
	case ev.NotAvailable:
		return "Not available"
	case ev.DoorOpen:
		return "Door open"
	case ev.PaperJam:
		return "Paper jam"
	case ev.PaperOut:
		return "Paper out"
	case ev.NoToner:
		return "Out of toner"
	case ev.UserIntervention:
		return "User intervention required"
	case ev.ErrorBit:
		return "Error"
	default:
		return ""
	}
}

// writeTracker remembers when each printer last completed a raw write.
type writeTracker struct {
	mu   sync.RWMutex
	last map[string]time.Time
}

var writeLog = &writeTracker{last: make(map[string]time.Time)}

// RecordSuccessfulWrite marks a printer as having completed a raw write. This is
// ground truth: it overrides every spooler status flag for WriteTrustWindow.
func RecordSuccessfulWrite(printerName string) {
	if printerName == "" {
		return
	}

	writeLog.mu.Lock()
	writeLog.last[printerName] = time.Now()
	writeLog.mu.Unlock()
}

// LastSuccessfulWrite returns when a printer last completed a raw write.
func LastSuccessfulWrite(printerName string) time.Time {
	writeLog.mu.RLock()
	defer writeLog.mu.RUnlock()
	return writeLog.last[printerName]
}
