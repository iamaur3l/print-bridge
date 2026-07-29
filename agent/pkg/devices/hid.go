package devices

type HIDDeviceInfo struct {
	VendorID    uint16 `json:"vendor_id"`
	ProductID   uint16 `json:"product_id"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Product      string `json:"product,omitempty"`
	Path         string `json:"path"`
}

// ListHIDDevices enumerates connected USB HID barcode scanners & card readers.
func ListHIDDevices() ([]HIDDeviceInfo, error) {
	// Stub HID enumeration list returning empty or mock items when HID hardware is queried
	return []HIDDeviceInfo{}, nil
}
