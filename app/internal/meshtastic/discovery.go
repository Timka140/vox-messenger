package meshtastic

import (
	"fmt"
	"strings"

	"go.bug.st/serial/enumerator"
)

// PortInfo describes a serial port surfaced for the user to pick.
// Friendly is a human label like "Heltec V3 (COM5)" suitable for direct
// display in a settings dropdown.
type PortInfo struct {
	Name             string `json:"name"`             // e.g. "COM5" or "/dev/ttyUSB0"
	Friendly         string `json:"friendly"`         // e.g. "Heltec V3 (COM5)"
	VID              string `json:"vid,omitempty"`    // hex, no prefix
	PID              string `json:"pid,omitempty"`    // hex, no prefix
	LikelyMeshtastic bool   `json:"likelyMeshtastic"` // true if VID matches a known LoRa-board USB chip
}

// usbVendor maps known USB-UART vendor IDs to the LoRa board family typically
// using that chip. Hits are heuristic — board manufacturers swap chips
// freely. Used only for friendly-name display, not validation.
var usbVendor = map[string]string{
	"10C4": "Heltec / Silicon Labs CP210x", // CP2102 — most Heltec V3, RAK, generic LoRa
	"1A86": "WCH CH340/CH9102",             // some Heltec older, Wemos, generic ESP32
	"303A": "Espressif ESP32 native USB",   // LilyGo T-Beam Supreme, Heltec Wireless Tracker
	"0403": "FTDI FT232",                   // older custom LoRa boards
}

// ListSerialPorts returns all currently visible serial ports, with friendly
// labels derived from USB VID/PID where available. The caller should
// re-invoke this on every settings panel open — boards may be plugged or
// unplugged at any time.
func ListSerialPorts() ([]PortInfo, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, fmt.Errorf("enumerate serial ports: %w", err)
	}
	out := make([]PortInfo, 0, len(ports))
	for _, p := range ports {
		info := PortInfo{Name: p.Name}
		if p.IsUSB {
			info.VID = strings.ToUpper(p.VID)
			info.PID = strings.ToUpper(p.PID)
			if vendor, ok := usbVendor[info.VID]; ok {
				info.LikelyMeshtastic = true
				info.Friendly = fmt.Sprintf("%s (%s)", vendor, p.Name)
			} else if p.Product != "" {
				info.Friendly = fmt.Sprintf("%s (%s)", p.Product, p.Name)
			}
		}
		if info.Friendly == "" {
			info.Friendly = p.Name
		}
		out = append(out, info)
	}
	return out, nil
}
