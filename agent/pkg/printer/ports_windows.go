//go:build windows

package printer

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// deviceEnumRoot is where Windows keeps the device tree that maps hardware to ports.
const deviceEnumRoot = `SYSTEM\CurrentControlSet\Enum`

// busesWithPortMappings lists the device buses that mint printer ports. USB covers
// practically every thermal printer; the others cover USB-to-parallel adapters and
// driver-created root devices.
var busesWithPortMappings = map[string]bool{
	"USB":     true,
	"USBSTOR": true,
	"LPTENUM": true,
	"ROOT":    true,
}

// LivePortMappings enumerates Windows' live device→port map, which is what makes a
// printer that has been moved to a different USB socket findable. Windows records it
// under
//
//	HKLM\SYSTEM\CurrentControlSet\Enum\<bus>\<hardware id>\<instance>\Device Parameters\PortName
//
// The hardware id (e.g. VID_04B8&PID_0E15&MI_00) is stable across sockets, which is
// what makes migration detectable; the instance is not.
func LivePortMappings() ([]LivePortMapping, error) {
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, deviceEnumRoot, registry.READ)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", deviceEnumRoot, err)
	}
	defer root.Close()

	buses, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return nil, fmt.Errorf("failed to list device buses: %w", err)
	}

	var mappings []LivePortMapping
	for _, bus := range buses {
		if !busesWithPortMappings[strings.ToUpper(bus)] {
			continue
		}
		mappings = append(mappings, mappingsForBus(root, bus)...)
	}

	sort.Slice(mappings, func(i, j int) bool { return mappings[i].PortName < mappings[j].PortName })
	return mappings, nil
}

func mappingsForBus(root registry.Key, bus string) []LivePortMapping {
	busKey, err := registry.OpenKey(root, bus, registry.READ)
	if err != nil {
		return nil
	}
	deviceIDs, err := busKey.ReadSubKeyNames(-1)
	busKey.Close()
	if err != nil {
		return nil
	}

	var mappings []LivePortMapping
	for _, deviceID := range deviceIDs {
		deviceKey, err := registry.OpenKey(root, bus+`\`+deviceID, registry.READ)
		if err != nil {
			continue
		}
		instances, err := deviceKey.ReadSubKeyNames(-1)
		deviceKey.Close()
		if err != nil {
			continue
		}

		for _, instance := range instances {
			port, ok := readPortName(root, fmt.Sprintf(`%s\%s\%s\Device Parameters`, bus, deviceID, instance))
			if !ok {
				continue
			}

			mappings = append(mappings, LivePortMapping{
				PortName: port,
				DeviceID: deviceID,
				Instance: fmt.Sprintf(`%s\%s\%s`, bus, deviceID, instance),
			})
		}
	}

	return mappings
}

// readPortName reads the REG_SZ PortName value, tolerating the trailing NUL that
// some drivers write.
func readPortName(root registry.Key, paramsPath string) (string, bool) {
	params, err := registry.OpenKey(root, paramsPath, registry.READ|registry.WOW64_64KEY)
	if err != nil {
		return "", false
	}
	defer params.Close()

	port, _, err := params.GetStringValue("PortName")
	if err != nil {
		return "", false
	}

	port = strings.TrimSpace(strings.Trim(port, "\x00"))
	if port == "" {
		return "", false
	}
	return port, true
}
