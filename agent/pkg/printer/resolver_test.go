package printer

import (
	"strings"
	"testing"
)

// testDeviceID stands in for a real hardware id such as "VID_04B8&PID_0E15&MI_00".
const testDeviceID = "VID_04B8&PID_0E15&MI_00"

func TestIsDevicePort(t *testing.T) {
	cases := map[string]bool{
		"USB001":          true,
		"usb011":          true,
		"DOT4_001":        true,
		"LPT1":            true,
		"IP_192.168.1.50": false,
		"WSD-3f2a":        false,
		"PORTPROMPT:":     false,
		"CUPS":            false,
		"":                false,
	}

	for port, want := range cases {
		if got := IsDevicePort(port); got != want {
			t.Errorf("IsDevicePort(%q) = %t, want %t", port, got, want)
		}
	}
}

func TestPlanPortBindingsPortPresentNeedsNothing(t *testing.T) {
	bindings := []PortBinding{{PrinterName: "Receipt", PortName: "USB003", DeviceID: testDeviceID}}
	live := []LivePortMapping{{PortName: "USB003", DeviceID: testDeviceID}}

	resolutions := PlanPortBindings(bindings, live)
	if len(resolutions) != 1 {
		t.Fatalf("expected one resolution, got %d", len(resolutions))
	}

	res := resolutions[0]
	if res.Action != ActionNone {
		t.Errorf("expected %q for a present port, got %q (%s)", ActionNone, res.Action, res.Reason)
	}
	if res.NeedsAttention() {
		t.Error("a present port must not require attention")
	}
	if res.DeviceID != testDeviceID {
		t.Errorf("expected the observed device identity to be reported, got %q", res.DeviceID)
	}
}

// TestPlanPortBindingsFollowsTheCable is the headline behaviour: the printer was
// moved to another USB socket, Windows minted a new port, and the queue should
// follow it instead of silently printing nowhere.
func TestPlanPortBindingsFollowsTheCable(t *testing.T) {
	bindings := []PortBinding{{PrinterName: "Receipt", PortName: "USB003", DeviceID: testDeviceID}}
	live := []LivePortMapping{{PortName: "USB011", DeviceID: testDeviceID}}

	res := PlanPortBindings(bindings, live)[0]
	if res.Action != ActionRebind {
		t.Fatalf("expected %q, got %q (%s)", ActionRebind, res.Action, res.Reason)
	}
	if res.NewPort != "USB011" {
		t.Errorf("expected the queue to follow to USB011, got %q", res.NewPort)
	}
	if res.CurrentPort != "USB003" {
		t.Errorf("expected the old port to be reported, got %q", res.CurrentPort)
	}
	if !strings.Contains(res.Reason, "moved from USB003 to USB011") {
		t.Errorf("expected an explanatory reason, got %q", res.Reason)
	}
}

func TestPlanPortBindingsRefusesToGuessBetweenIdenticalDevices(t *testing.T) {
	// Two identical units on one hub share a device identity, so the agent cannot
	// tell which queue belongs to which socket.
	bindings := []PortBinding{{PrinterName: "Kitchen", PortName: "USB003", DeviceID: testDeviceID}}
	live := []LivePortMapping{
		{PortName: "USB011", DeviceID: testDeviceID},
		{PortName: "USB012", DeviceID: testDeviceID},
	}

	res := PlanPortBindings(bindings, live)[0]
	if res.Action != ActionAmbiguous {
		t.Fatalf("expected %q, got %q (%s)", ActionAmbiguous, res.Action, res.Reason)
	}
	if res.NewPort != "" {
		t.Errorf("an ambiguous resolution must not propose a port, got %q", res.NewPort)
	}
	if !strings.Contains(res.Reason, "refusing to guess") {
		t.Errorf("expected the refusal to be explicit, got %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "2 ports") {
		t.Errorf("expected the candidate count in the reason, got %q", res.Reason)
	}
}

// TestPlanPortBindingsNeverStealsAnotherQueuesPort guards against a kitchen ticket
// being hijacked by the receipt printer when both record the same device identity.
func TestPlanPortBindingsNeverStealsAnotherQueuesPort(t *testing.T) {
	bindings := []PortBinding{
		{PrinterName: "Receipt", PortName: "USB003", DeviceID: testDeviceID},
		{PrinterName: "Kitchen", PortName: "USB007", DeviceID: testDeviceID},
	}
	live := []LivePortMapping{{PortName: "USB007", DeviceID: testDeviceID}}

	resolutions := PlanPortBindings(bindings, live)
	receipt, kitchen := resolutions[0], resolutions[1]

	if receipt.Action != ActionMissing {
		t.Errorf("expected %q for the queue that lost its port, got %q (%s)", ActionMissing, receipt.Action, receipt.Reason)
	}
	if receipt.NewPort != "" {
		t.Errorf("must not propose a port owned by another queue, got %q", receipt.NewPort)
	}
	if !strings.Contains(receipt.Reason, "already belongs to another queue") {
		t.Errorf("expected the ownership conflict to be explained, got %q", receipt.Reason)
	}

	if kitchen.Action != ActionNone {
		t.Errorf("the owning queue should be unaffected, got %q (%s)", kitchen.Action, kitchen.Reason)
	}
}

func TestPlanPortBindingsDeviceDisconnected(t *testing.T) {
	bindings := []PortBinding{{PrinterName: "Receipt", PortName: "USB003", DeviceID: testDeviceID}}

	res := PlanPortBindings(bindings, nil)[0]
	if res.Action != ActionMissing {
		t.Fatalf("expected %q, got %q (%s)", ActionMissing, res.Action, res.Reason)
	}
	if !strings.Contains(res.Reason, "no device matching") {
		t.Errorf("expected a disconnection reason, got %q", res.Reason)
	}
}

func TestPlanPortBindingsWithoutRecordedIdentity(t *testing.T) {
	// A queue that has never been observed with its device present cannot be
	// identified later, so the agent reports rather than guesses.
	bindings := []PortBinding{{PrinterName: "Receipt", PortName: "USB003"}}

	res := PlanPortBindings(bindings, nil)[0]
	if res.Action != ActionMissing {
		t.Fatalf("expected %q, got %q", ActionMissing, res.Action)
	}
	if !strings.Contains(res.Reason, "no device identity was recorded") {
		t.Errorf("expected the missing identity to be explained, got %q", res.Reason)
	}
}

func TestPlanPortBindingsSkipsNonDevicePorts(t *testing.T) {
	bindings := []PortBinding{{PrinterName: "Back office", PortName: "IP_10.0.0.5"}}

	res := PlanPortBindings(bindings, nil)[0]
	if res.Action != ActionNone {
		t.Errorf("network ports cannot migrate, expected %q, got %q (%s)", ActionNone, res.Action, res.Reason)
	}
	if res.NeedsAttention() {
		t.Error("an untracked network port must not raise attention")
	}
	if !strings.Contains(res.Reason, "does not follow movable hardware") {
		t.Errorf("expected an explanation, got %q", res.Reason)
	}
}

func TestObserveBindingsRecordsIdentityOnlyForDevicePorts(t *testing.T) {
	printers := []PrinterInfo{
		{Name: "Receipt", PortName: "USB003"},
		{Name: "Back office", PortName: "IP_10.0.0.5"},
		{Name: "Virtual", PortName: "PORTPROMPT:"},
	}
	live := []LivePortMapping{{PortName: "usb003", DeviceID: testDeviceID}}

	bindings := ObserveBindings(printers, live)
	if len(bindings) != 1 {
		t.Fatalf("expected only the device-backed queue to be recorded, got %+v", bindings)
	}
	if bindings[0].PrinterName != "Receipt" || bindings[0].PortName != "USB003" {
		t.Errorf("unexpected binding: %+v", bindings[0])
	}
	// Port comparison must be case-insensitive: the registry casing varies.
	if bindings[0].DeviceID != testDeviceID {
		t.Errorf("expected the device identity to be attached, got %q", bindings[0].DeviceID)
	}
}

func TestSummariseResolutions(t *testing.T) {
	if got := SummariseResolutions(nil); got != "all printer ports are present" {
		t.Errorf("unexpected empty summary: %q", got)
	}

	resolutions := []Resolution{
		{PrinterName: "Receipt", Action: ActionNone, Reason: "port USB003 is present"},
		{PrinterName: "Kitchen", Action: ActionRebind, Reason: "device moved from USB003 to USB011"},
	}
	got := SummariseResolutions(resolutions)
	if !strings.Contains(got, "Kitchen: device moved from USB003 to USB011") {
		t.Errorf("expected only unresolved printers in the summary, got %q", got)
	}
	if strings.Contains(got, "Receipt") {
		t.Errorf("a healthy printer should not appear in the summary, got %q", got)
	}
}