package printer

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseNetworkPort(t *testing.T) {
	cases := []struct {
		port     string
		wantHost string
		wantPort int
		wantOK   bool
	}{
		{"IP_192.168.1.100", "192.168.1.100", 9100, true},
		{"ip_10.0.0.5", "10.0.0.5", 9100, true},
		{"IP_10.0.0.5_1", "10.0.0.5", 9100, true},
		{"IP_192.168.1.100:9101", "192.168.1.100", 9101, true},
		{"10.0.0.5", "10.0.0.5", 9100, true},
		{"printer.local", "printer.local", 9100, true},
		{"kitchen-printer:515", "kitchen-printer", 515, true},
		{"[fe80::1]:9100", "fe80::1", 9100, true},

		// Everything below must stay on the spooler path.
		{"", "", 0, false},
		{"USB001", "", 0, false},
		{"DOT4_001", "", 0, false},
		{"LPT1", "", 0, false},
		{"COM3", "", 0, false},
		{"WSD-3f2a1b7c-1d2e-4f56-8a9b-0c1d2e3f4a5b", "", 0, false},
		{"PORTPROMPT:", "", 0, false},
		{"SHRFAX:", "", 0, false},
		{"CUPS", "", 0, false},
		{"URL_192.168.1.100", "", 0, false},
		{"C:\\temp\\print.raw", "", 0, false},
		{"IP_", "", 0, false},
		{"192.168.1.100:notaport", "", 0, false},
		{"192.168.1.100:99999", "", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.port, func(t *testing.T) {
			target, ok := ParseNetworkPort(tc.port)
			if ok != tc.wantOK {
				t.Fatalf("ParseNetworkPort(%q) ok = %t, want %t", tc.port, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if target.Host != tc.wantHost || target.Port != tc.wantPort {
				t.Errorf("ParseNetworkPort(%q) = %s, want %s:%d",
					tc.port, target.Address(), tc.wantHost, tc.wantPort)
			}
		})
	}
}

// TestSendRawToNetworkDeliversBytes exercises the real socket path end to end against
// a local listener standing in for a thermal printer.
func TestSendRawToNetworkDeliversBytes(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	target := NetworkTarget{Host: "127.0.0.1", Port: port}

	received := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, 0, 256)
		chunk := make([]byte, 128)
		for {
			n, err := conn.Read(chunk)
			buf = append(buf, chunk[:n]...)
			if err != nil {
				break
			}
		}
		received <- buf
	}()

	payload := []byte{0x1B, 0x40, 'H', 'e', 'l', 'l', 'o', 0x0A, 0x1D, 0x56, 0x41, 0x00}
	printerName := fmt.Sprintf("Network Test Printer %d", port)

	if err := SendRawToNetwork(printerName, target, payload, 3*time.Second); err != nil {
		t.Fatalf("SendRawToNetwork failed: %v", err)
	}

	select {
	case got := <-received:
		if string(got) != string(payload) {
			t.Errorf("printer received %v, want %v", got, payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the listener to receive the job")
	}

	// A completed network write is ground truth for the health engine.
	if LastSuccessfulWrite(printerName).IsZero() {
		t.Error("expected a successful network write to be recorded")
	}
}

func TestSendRawToNetworkConnectionRefused(t *testing.T) {
	// Bind and immediately release a port so nothing is listening on it.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	target := NetworkTarget{Host: "127.0.0.1", Port: port}
	err = SendRawToNetwork("Refused Printer", target, []byte("payload"), 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected a dial failure when nothing is listening")
	}

	// A dial failure wrote nothing, so the caller may safely fall back.
	var partial *PartialWriteError
	if errors.As(err, &partial) {
		t.Errorf("a connection failure must not look like a partial write: %v", err)
	}
}

func TestProbeNetworkTarget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if err := ProbeNetworkTarget(NetworkTarget{Host: "127.0.0.1", Port: port}, time.Second); err != nil {
		t.Errorf("expected a reachable target to probe clean, got %v", err)
	}

	_ = listener.Close()

	if err := ProbeNetworkTarget(NetworkTarget{Host: "127.0.0.1", Port: port}, 300*time.Millisecond); err == nil {
		t.Error("expected a closed listener to probe as unreachable")
	}
}

func TestRouterTransportFor(t *testing.T) {
	router := NewRouter(func() ([]PrinterInfo, error) {
		return []PrinterInfo{
			{Name: "Kitchen", PortName: "IP_192.168.1.100"},
			{Name: "Receipt", PortName: "USB003"},
			{Name: "PDF", PortName: "PORTPROMPT:"},
			{Name: "Explicit", PortName: "10.0.0.7:515"},
		}, nil
	}, time.Second)

	cases := map[string]Transport{
		"Kitchen":  TransportNetwork,
		"Receipt":  TransportSpooler,
		"PDF":      TransportSpooler,
		"Explicit": TransportNetwork,
		"Unknown":  TransportSpooler,
	}

	for name, want := range cases {
		if got, _ := router.TransportFor(name); got != want {
			t.Errorf("TransportFor(%q) = %q, want %q", name, got, want)
		}
	}

	// Case-insensitive queue matching, because Windows queue names vary in case.
	if got, target := router.TransportFor("kitchen"); got != TransportNetwork || target.Host != "192.168.1.100" {
		t.Errorf("expected a case-insensitive match to the network queue, got %q %+v", got, target)
	}

	if _, target := router.TransportFor("Explicit"); target.Port != 515 {
		t.Errorf("expected an explicit port to be honoured, got %d", target.Port)
	}

	// A failing enumeration must not break printing: fall back to the spooler.
	broken := NewRouter(func() ([]PrinterInfo, error) { return nil, fmt.Errorf("enumeration failed") }, time.Second)
	if got, _ := broken.TransportFor("Kitchen"); got != TransportSpooler {
		t.Errorf("expected the spooler when enumeration fails, got %q", got)
	}
}

func TestPartialWriteErrorUnwraps(t *testing.T) {
	root := fmt.Errorf("connection reset")
	partial := &PartialWriteError{Addr: "10.0.0.5:9100", Written: 12, Total: 200, Err: root}

	if !errors.Is(partial, root) {
		t.Error("expected PartialWriteError to unwrap to its cause")
	}

	msg := partial.Error()
	if !strings.Contains(msg, "12/200") || !strings.Contains(msg, "10.0.0.5:9100") {
		t.Errorf("expected the message to describe the partial write, got %q", msg)
	}
}

// TestNetworkReachabilityVerdicts covers the transport-probe branches of the health
// engine: a network printer that answers is online even if the spooler disagrees,
// and one that refuses is genuinely offline.
func TestNetworkReachabilityVerdicts(t *testing.T) {
	now := time.Now()

	reachable := true
	verdict := EvaluateStatus(StatusEvidence{
		WorkOfflineAttr: true,
		OfflineBit:      true,
		Reachable:       &reachable,
	}, now)
	if verdict.State != StateOnline {
		t.Errorf("a reachable printer should be online, got %q (%s)", verdict.State, verdict.Detail)
	}
	if !verdict.StaleFlags {
		t.Error("expected the contradicted spooler flags to be reported as stale")
	}

	unreachable := false
	verdict = EvaluateStatus(StatusEvidence{Reachable: &unreachable}, now)
	if verdict.State != StateOffline {
		t.Errorf("an unreachable printer should be offline, got %q", verdict.State)
	}
	if verdict.Description != "Not reachable" {
		t.Errorf("expected a reachability description, got %q", verdict.Description)
	}

	// A hardware fault still outranks a successful probe.
	verdict = EvaluateStatus(StatusEvidence{PaperOut: true, Reachable: &reachable}, now)
	if verdict.State != StateOffline {
		t.Errorf("a paper-out fault must stay offline, got %q", verdict.State)
	}

	// An unprobed printer keeps the flag-based behaviour.
	verdict = EvaluateStatus(StatusEvidence{WorkOfflineAttr: true}, now)
	if verdict.State != StateUnknown {
		t.Errorf("expected the unprobed offlags case to stay %q, got %q", StateUnknown, verdict.State)
	}
}