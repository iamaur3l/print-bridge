package printer

import (
	"fmt"
	"sort"
	"strings"
)

// PortBinding is what the agent remembers about where a print queue lives.
type PortBinding struct {
	PrinterName string `json:"printer_name"`
	// PortName is the spooler port the queue is currently bound to, e.g. "USB011".
	PortName string `json:"port_name"`
	// DeviceID is the stable device identity observed while that port was present
	// (for example "VID_04B8&PID_0E15&MI_00"). Two identical printers share a
	// DeviceID, which is exactly why ambiguity must be reported, never guessed.
	DeviceID string `json:"device_id,omitempty"`
}

// LivePortMapping is one live device→port mapping read from the OS.
type LivePortMapping struct {
	PortName string `json:"port_name"`
	DeviceID string `json:"device_id"`
	Instance string `json:"instance,omitempty"`
}

// ResolutionAction is what the resolver recommends for one printer.
type ResolutionAction string

const (
	// ActionNone means the queue is bound to a port that is present.
	ActionNone ResolutionAction = "none"
	// ActionRebind means the device reappeared on exactly one other port and the
	// queue should follow it.
	ActionRebind ResolutionAction = "rebind"
	// ActionAmbiguous means several ports could host this device, so the agent
	// refuses to guess: sending order tickets to the wrong station is worse than
	// asking.
	ActionAmbiguous ResolutionAction = "ambiguous"
	// ActionMissing means nothing plausible is currently connected.
	ActionMissing ResolutionAction = "missing"
)

// Resolution is the diagnosis for one printer.
type Resolution struct {
	PrinterName string           `json:"printer_name"`
	Action      ResolutionAction `json:"action"`
	CurrentPort string           `json:"current_port,omitempty"`
	NewPort     string           `json:"new_port,omitempty"`
	DeviceID    string           `json:"device_id,omitempty"`
	Reason      string           `json:"reason"`
}

// NeedsAttention reports whether a human (or a repair) has to do something.
func (r Resolution) NeedsAttention() bool {
	return r.Action != ActionNone
}

// IsDevicePort reports whether a spooler port follows physically movable hardware.
// USB001 and DOT4_001 do; IP_10.0.0.5, WSD-, PORTPROMPT: and CUPS do not.
func IsDevicePort(portName string) bool {
	up := strings.ToUpper(strings.TrimSpace(portName))
	return strings.HasPrefix(up, "USB") ||
		strings.HasPrefix(up, "LPT") ||
		strings.HasPrefix(up, "DOT4")
}

// ObserveBindings builds the bindings worth remembering from the current queues and
// the live device map. Non-device ports are skipped because they cannot migrate.
func ObserveBindings(printers []PrinterInfo, live []LivePortMapping) []PortBinding {
	deviceByPort := make(map[string]string, len(live))
	for _, m := range live {
		if m.PortName == "" {
			continue
		}
		deviceByPort[strings.ToUpper(m.PortName)] = m.DeviceID
	}

	bindings := make([]PortBinding, 0, len(printers))
	for _, p := range printers {
		if !IsDevicePort(p.PortName) {
			continue
		}
		bindings = append(bindings, PortBinding{
			PrinterName: p.Name,
			PortName:    p.PortName,
			DeviceID:    deviceByPort[strings.ToUpper(p.PortName)],
		})
	}
	return bindings
}

// PlanPortBindings decides how each queue should be bound.
//
// The rules exist because moving a USB cable mints a brand new Windows port while
// the print queue stays pinned to the old one, which silently breaks printing:
//
//  1. A queue whose port is still present needs nothing.
//  2. A queue whose device reappeared on exactly one other port is moved there.
//  3. A queue whose device could be on several ports is reported as ambiguous
//     instead of guessed (two identical units on one hub share an identity).
//  4. A port already owned by another configured queue is never taken over, so a
//     kitchen ticket cannot be hijacked by the receipt printer.
//  5. A queue with no recorded device identity is reported, not guessed at.
func PlanPortBindings(bindings []PortBinding, live []LivePortMapping) []Resolution {
	livePorts := make(map[string]bool, len(live))
	byDevice := make(map[string][]LivePortMapping, len(live))
	for _, m := range live {
		if m.PortName == "" {
			continue
		}
		livePorts[strings.ToUpper(m.PortName)] = true
		byDevice[m.DeviceID] = append(byDevice[m.DeviceID], m)
	}

	owner := make(map[string]string, len(bindings))
	for _, b := range bindings {
		if b.PortName != "" {
			owner[strings.ToUpper(b.PortName)] = b.PrinterName
		}
	}

	resolutions := make([]Resolution, 0, len(bindings))
	for _, b := range bindings {
		resolutions = append(resolutions, planOneBinding(b, livePorts, byDevice, owner))
	}
	return resolutions
}

func planOneBinding(b PortBinding, livePorts map[string]bool, byDevice map[string][]LivePortMapping, owner map[string]string) Resolution {
	res := Resolution{PrinterName: b.PrinterName, CurrentPort: b.PortName}

	if !IsDevicePort(b.PortName) {
		res.Action = ActionNone
		res.Reason = fmt.Sprintf("port %q does not follow movable hardware, so it is not tracked", b.PortName)
		return res
	}

	if livePorts[strings.ToUpper(b.PortName)] {
		res.Action = ActionNone
		res.DeviceID = deviceIDForPort(b.PortName, byDevice, b.DeviceID)
		res.Reason = fmt.Sprintf("port %s is present", b.PortName)
		return res
	}

	// The configured port is gone from the machine.
	if b.DeviceID == "" {
		res.Action = ActionMissing
		res.Reason = fmt.Sprintf("port %s is gone and no device identity was recorded, so the agent cannot tell which device this queue belongs to", b.PortName)
		return res
	}
	res.DeviceID = b.DeviceID

	candidates := append([]LivePortMapping(nil), byDevice[b.DeviceID]...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].PortName < candidates[j].PortName })

	// A candidate port owned by a different queue is out of bounds, however
	// tempting it looks.
	free := make([]LivePortMapping, 0, len(candidates))
	for _, c := range candidates {
		if other, taken := owner[strings.ToUpper(c.PortName)]; taken && other != b.PrinterName {
			continue
		}
		free = append(free, c)
	}

	switch {
	case len(free) > 1:
		res.Action = ActionAmbiguous
		res.Reason = fmt.Sprintf("%d ports could host device %s (%s); refusing to guess which one this queue belongs to",
			len(free), b.DeviceID, joinPorts(free))

	case len(free) == 0:
		res.Action = ActionMissing
		if len(candidates) > 0 {
			res.Reason = fmt.Sprintf("device %s is now on %s, but that port already belongs to another queue",
				b.DeviceID, joinPorts(candidates))
		} else {
			res.Reason = fmt.Sprintf("no device matching %s is currently connected", b.DeviceID)
		}

	default:
		res.Action = ActionRebind
		res.NewPort = free[0].PortName
		res.Reason = fmt.Sprintf("device %s moved from %s to %s", b.DeviceID, b.PortName, free[0].PortName)
	}

	return res
}

func deviceIDForPort(portName string, byDevice map[string][]LivePortMapping, fallback string) string {
	for deviceID, mappings := range byDevice {
		if deviceID == "" {
			continue
		}
		for _, m := range mappings {
			if strings.EqualFold(m.PortName, portName) {
				return deviceID
			}
		}
	}
	return fallback
}

func joinPorts(mappings []LivePortMapping) string {
	names := make([]string, 0, len(mappings))
	for _, m := range mappings {
		names = append(names, m.PortName)
	}
	return strings.Join(names, ", ")
}

// SummariseResolutions renders a one-line operator summary of a diagnosis.
func SummariseResolutions(resolutions []Resolution) string {
	attention := make([]string, 0, len(resolutions))
	for _, r := range resolutions {
		if !r.NeedsAttention() {
			continue
		}
		attention = append(attention, fmt.Sprintf("%s: %s", r.PrinterName, r.Reason))
	}
	if len(attention) == 0 {
		return "all printer ports are present"
	}
	return strings.Join(attention, "; ")
}