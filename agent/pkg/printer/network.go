package printer

import (
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

// DefaultRawPort is the IANA-registered RAW/JetDirect printing port.
const DefaultRawPort = 9100

// DefaultNetworkTimeout bounds a network print, and DefaultProbeTimeout bounds a
// reachability probe (which runs inside the cached snapshot refresh, so it must be
// short).
const (
	DefaultNetworkTimeout = 10 * time.Second
	DefaultProbeTimeout   = 600 * time.Millisecond
)

// NetworkTarget is a raw-socket printer address.
type NetworkTarget struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Address renders the target as host:port.
func (t NetworkTarget) Address() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

// String implements fmt.Stringer.
func (t NetworkTarget) String() string { return t.Address() }

// nonNetworkPortPrefixes are spooler port families that never address a TCP host.
// URL_/TCPMON are deliberately included: their semantics are not documented well
// enough to route on, so those queues keep the historical spooler path.
var nonNetworkPortPrefixes = []string{
	"USB", "LPT", "COM", "DOT4", "WSD", "PORTPROMPT", "SHRFAX", "FILE:", "NUL",
	"CUPS", "PRINTTO", "RPT", "LPR", "TCPMON", "URL_", "\\",
}

func looksLikeHost(candidate string) bool {
	if candidate == "" || len(candidate) > 255 {
		return false
	}
	if strings.ContainsAny(candidate, " \t/\\*?<>|\"") {
		return false
	}

	up := strings.ToUpper(candidate)
	for _, prefix := range nonNetworkPortPrefixes {
		if strings.HasPrefix(up, prefix) {
			return false
		}
	}
	return true
}

// ParseNetworkPort extracts a TCP target from a spooler port name. It recognises the
// Windows "Standard TCP/IP Port" form ("IP_192.168.1.100", including the "_1" suffix
// Windows appends to duplicated ports), bare addresses ("10.0.0.5",
// "printer.local:9100") and bracketed IPv6 ("[fe80::1]"). Device, spooler and
// virtual ports (USB001, LPT1, COM3, DOT4_001, WSD-…, PORTPROMPT:, SHRFAX:, CUPS)
// return false.
func ParseNetworkPort(portName string) (NetworkTarget, bool) {
	name := strings.TrimSpace(portName)
	if name == "" {
		return NetworkTarget{}, false
	}

	if strings.HasPrefix(strings.ToUpper(name), "IP_") {
		name = name[3:]
	} else if !looksLikeHost(name) {
		return NetworkTarget{}, false
	}

	// Windows disambiguates duplicate ports as "IP_10.0.0.5_1".
	if idx := strings.LastIndex(name, "_"); idx > 0 && !strings.Contains(name, ":") {
		if _, err := strconv.Atoi(name[idx+1:]); err == nil {
			name = name[:idx]
		}
	}

	host := name
	port := DefaultRawPort

	if strings.Contains(name, ":") {
		h, p, err := net.SplitHostPort(name)
		if err != nil {
			return NetworkTarget{}, false
		}
		parsed, err := strconv.Atoi(p)
		if err != nil || parsed <= 0 || parsed > 65535 {
			return NetworkTarget{}, false
		}
		host, port = h, parsed
	}

	host = strings.Trim(host, "[]")
	if !looksLikeHost(host) {
		return NetworkTarget{}, false
	}

	return NetworkTarget{Host: host, Port: port}, true
}

// PartialWriteError reports a network write that failed after the printer had
// already accepted some bytes. Retrying such a job elsewhere can duplicate a
// receipt, so callers must not silently fall back to another transport.
type PartialWriteError struct {
	Addr    string
	Written int
	Total   int
	Err     error
}

func (e *PartialWriteError) Error() string {
	return fmt.Sprintf("network print to %s failed after %d/%d bytes: %v", e.Addr, e.Written, e.Total, e.Err)
}

// Unwrap exposes the underlying transport error.
func (e *PartialWriteError) Unwrap() error { return e.Err }

// SendRawToNetwork writes a raw byte stream straight to a network printer's
// RAW/JetDirect port, bypassing the OS spooler and its offline-flag behaviour. A
// completed write is recorded as ground truth for the named printer.
func SendRawToNetwork(printerName string, target NetworkTarget, data []byte, timeout time.Duration) error {
	if err := VerifyPayload(data); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = DefaultNetworkTimeout
	}

	addr := target.Address()

	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("failed to connect to network printer %s (%s): %w", printerName, addr, err)
	}
	defer conn.Close()

	_ = conn.SetWriteDeadline(time.Now().Add(timeout))

	written, err := conn.Write(data)
	if err != nil {
		return &PartialWriteError{Addr: addr, Written: written, Total: len(data), Err: err}
	}
	if written != len(data) {
		return &PartialWriteError{Addr: addr, Written: written, Total: len(data), Err: errors.New("short write")}
	}

	RecordSuccessfulWrite(printerName)
	return nil
}

// ProbeNetworkTarget reports whether a TCP connection to the printer's RAW port can
// be established. A successful probe is genuine corroboration that the device is
// present, which is what lets a stale spooler offline flag be overridden.
func ProbeNetworkTarget(target NetworkTarget, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}

	conn, err := net.DialTimeout("tcp", target.Address(), timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Transport names how a queue is printed to.
type Transport string

const (
	// TransportSpooler goes through the OS print queue (winspool / CUPS).
	TransportSpooler Transport = "spooler"
	// TransportNetwork writes directly to the printer's TCP RAW port.
	TransportNetwork Transport = "network"
)

// Router sends each job over the best available transport for its target queue.
// Network printers whose spooler port is a TCP address are written to directly,
// which removes the spooler (and its stale offline flags) from the path entirely;
// everything else keeps going through the OS queue.
type Router struct {
	list    func() ([]PrinterInfo, error)
	timeout time.Duration
}

// NewRouter builds a Router over a printer list function (use ListPrintersCached so
// routing shares the agent-wide snapshot).
func NewRouter(list func() ([]PrinterInfo, error), timeout time.Duration) *Router {
	if list == nil {
		list = ListPrintersCached
	}
	if timeout <= 0 {
		timeout = DefaultNetworkTimeout
	}
	return &Router{list: list, timeout: timeout}
}

// TransportFor reports how a queue should be printed to.
func (r *Router) TransportFor(printerName string) (Transport, NetworkTarget) {
	printers, err := r.list()
	if err != nil {
		return TransportSpooler, NetworkTarget{}
	}

	for _, p := range printers {
		if strings.EqualFold(p.Name, printerName) {
			if target, ok := ParseNetworkPort(p.PortName); ok {
				return TransportNetwork, target
			}
			return TransportSpooler, NetworkTarget{}
		}
	}

	// Unknown queue: keep the historical behaviour and let the spooler answer.
	return TransportSpooler, NetworkTarget{}
}

// Print implements queue.PrintFunc.
func (r *Router) Print(printerName string, data []byte, jobName string) error {
	transport, target := r.TransportFor(printerName)
	if transport != TransportNetwork {
		return PrintRaw(printerName, data, jobName)
	}

	err := SendRawToNetwork(printerName, target, data, r.timeout)
	if err == nil {
		return nil
	}

	// Fall back to the spooler only when nothing reached the printer at all: after a
	// partial write, printing the job again would duplicate a receipt.
	var partial *PartialWriteError
	if errors.As(err, &partial) && partial.Written > 0 {
		return err
	}

	log.Printf("[Printer] Direct TCP write to %s failed (%v); falling back to the spooler queue %q",
		target.Address(), err, printerName)

	return PrintRaw(printerName, data, jobName)
}