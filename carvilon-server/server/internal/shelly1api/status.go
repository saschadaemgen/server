// GET /status - the Gen1 live-state read. Unlike Gen2's component map
// ("switch:0", "input:1"), Gen1 reports parallel ARRAYS - relays[],
// meters[], inputs[] - where the array index IS the channel id. Meters
// deserve care: energy counters are WATT-MINUTES (not Wh) and reset to 0
// on reboot (documented), and a non-metering Shelly 1 still reports a
// meters[] entry whose "power" is a user-set constant, not a measurement
// - the capability table (shellycaps), not the presence of a meter entry,
// decides whether a channel renders as metered.
package shelly1api

import (
	"context"
	"strconv"
	"strings"
)

// RelayStatus is one relays[] entry.
type RelayStatus struct {
	IsOn      flexVal `json:"ison"`
	Overpower flexVal `json:"overpower"` // metering models only
	Source    flexVal `json:"source"`    // origin of the last command
}

// MeterStatus is one meters[] entry.
type MeterStatus struct {
	Power   flexVal `json:"power"` // W (Shelly 1: the user power constant)
	IsValid flexVal `json:"is_valid"`
	Total   flexVal `json:"total"` // WATT-MINUTES since boot (resets on reboot)
}

// InputStatus is one inputs[] entry (the physical switch terminal).
type InputStatus struct {
	Input    flexVal `json:"input"` // 0/1
	Event    flexVal `json:"event"` // "S"/"L"/""
	EventCnt flexVal `json:"event_cnt"`
}

// LightStatus is one lights[] entry of a light-class device (RGBW2).
// Confirmed on a real SHRGBW2: the per-light power (W) rides here, not
// only on the shared meter.
type LightStatus struct {
	IsOn       flexVal `json:"ison"`
	Source     flexVal `json:"source"`
	HasTimer   flexVal `json:"has_timer"`
	Mode       flexVal `json:"mode"`
	Red        flexVal `json:"red"`
	Green      flexVal `json:"green"`
	Blue       flexVal `json:"blue"`
	White      flexVal `json:"white"`
	Gain       flexVal `json:"gain"`
	Brightness flexVal `json:"brightness"`
	Effect     flexVal `json:"effect"`
	Transition flexVal `json:"transition"`
	Power      flexVal `json:"power"` // W
	Overpower  flexVal `json:"overpower"`
}

// TempStatus is the device temperature block (tmp) on models that report
// one (1PM, 2.5, Plug S).
type TempStatus struct {
	TC      flexVal `json:"tC"`
	IsValid flexVal `json:"is_valid"`
}

// Status is the subset of GET /status the Device Center renders.
type Status struct {
	Relays          []RelayStatus `json:"relays"`
	Lights          []LightStatus `json:"lights"`
	Meters          []MeterStatus `json:"meters"`
	Inputs          []InputStatus `json:"inputs"`
	Temperature     flexVal       `json:"temperature"` // °C scalar (fw-dependent)
	Tmp             TempStatus    `json:"tmp"`
	Overtemperature flexVal       `json:"overtemperature"`
	Voltage         flexVal       `json:"voltage"` // 2.5 only
	Uptime          flexVal       `json:"uptime"`
	HasUpdate       flexVal       `json:"has_update"`
	MQTT            struct {
		Connected flexVal `json:"connected"`
	} `json:"mqtt"`
	Update struct {
		Status     flexVal `json:"status"`
		HasUpdate  flexVal `json:"has_update"`
		NewVersion flexVal `json:"new_version"`
		OldVersion flexVal `json:"old_version"`
	} `json:"update"`
	WiFiSta struct {
		RSSI flexVal `json:"rssi"` // dBm - surfaced in the cockpit (a weak
		// signal is the likeliest cause of a flaky WiFi-only device)
	} `json:"wifi_sta"`
}

// GetStatus reads the device state (Basic auth when the device is
// protected).
func (c *Client) GetStatus(ctx context.Context) (*Status, error) {
	var st Status
	if err := c.getJSON(ctx, "/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// StateLabel renders a relay's state ("On"/"Off"/"-").
func (r RelayStatus) StateLabel() string {
	if v, ok := r.IsOn.Bool(); ok {
		if v {
			return "On"
		}
		return "Off"
	}
	return "-"
}

// StateLabel renders a light's state ("On"/"Off"/"-").
func (l LightStatus) StateLabel() string {
	if v, ok := l.IsOn.Bool(); ok {
		if v {
			return "On"
		}
		return "Off"
	}
	return "-"
}

// PowerLabel renders a light's own power reading ("12.4 W" / "-").
func (l LightStatus) PowerLabel() string {
	if v, ok := l.Power.Float(); ok {
		return trimFloat(v) + " W"
	}
	return "-"
}

// RSSILabel renders the WiFi signal ("-94 dBm" / "") - the cockpit
// surfaces it because a weak signal is the likeliest cause of a flaky
// WiFi-only device.
func (s *Status) RSSILabel() string {
	if v, ok := s.WiFiSta.RSSI.Float(); ok {
		return trimFloat(v) + " dBm"
	}
	return ""
}

// PowerLabel renders a meter's active power ("12.4 W" / "-").
func (m MeterStatus) PowerLabel() string {
	if v, ok := m.Power.Float(); ok {
		return trimFloat(v) + " W"
	}
	return "-"
}

// EnergyLabel renders a meter's total counter converted to Wh (the
// device counts watt-minutes; dividing by 60 is the honest unit for a
// UI, with the reboot-reset caveat living in the docs).
func (m MeterStatus) EnergyLabel() string {
	if v, ok := m.Total.Float(); ok {
		return trimFloat(v/60) + " Wh"
	}
	return "-"
}

// TempLabel renders the device temperature ("41.2 °C" / "").
func (s *Status) TempLabel() string {
	for _, f := range []flexVal{s.Tmp.TC, s.Temperature} {
		if v, ok := f.Float(); ok {
			return trimFloat(v) + " °C"
		}
	}
	return ""
}

// trimFloat formats a reading without trailing float noise.
func trimFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}
