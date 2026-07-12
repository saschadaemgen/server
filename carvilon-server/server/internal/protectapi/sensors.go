// Sensors (GET /v1/sensors) - Saison 21, Protect Etappe 1. The exact
// substructure of the sensor record (stats, batteryStatus,
// wirelessConnectionState) is only "probably" as documented and gets
// verified against the real NVR during the RPi check; every nested
// container therefore decodes tolerantly (a drifted shape reads as
// absent, never as a list-killing error) and every display accessor
// degrades to "" so the Device Center renders "-" instead of
// invented data.
package protectapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SensorStat is one measured channel inside stats (value + status).
type SensorStat struct {
	Value  flexVal `json:"value"`
	Status flexVal `json:"status"`
}

// UnmarshalJSON decodes best-effort: a drifted stats channel reads as
// absent instead of failing the sensor (and with it the whole list).
func (s *SensorStat) UnmarshalJSON(b []byte) error {
	type alias SensorStat
	var a alias
	if err := json.Unmarshal(b, &a); err == nil {
		*s = SensorStat(a)
	}
	return nil
}

// SensorStats is the stats block (temperature / humidity / light).
type SensorStats struct {
	Temperature SensorStat `json:"temperature"`
	Humidity    SensorStat `json:"humidity"`
	Light       SensorStat `json:"light"`
}

// UnmarshalJSON decodes best-effort (see SensorStat).
func (s *SensorStats) UnmarshalJSON(b []byte) error {
	type alias SensorStats
	var a alias
	if err := json.Unmarshal(b, &a); err == nil {
		*s = SensorStats(a)
	}
	return nil
}

// BatteryStatus is the batteryStatus block.
type BatteryStatus struct {
	Percentage flexVal `json:"percentage"`
	IsLow      flexVal `json:"isLow"`
}

// UnmarshalJSON decodes best-effort (see SensorStat).
func (b *BatteryStatus) UnmarshalJSON(data []byte) error {
	type alias BatteryStatus
	var a alias
	if err := json.Unmarshal(data, &a); err == nil {
		*b = BatteryStatus(a)
	}
	return nil
}

// WirelessConnectionState is the wirelessConnectionState block. The
// briefing names signalState / RSSI / bridge; the spellings here are
// candidates that get confirmed on the real NVR.
type WirelessConnectionState struct {
	SignalState    flexVal `json:"signalState"`
	SignalStrength flexVal `json:"signalStrength"`
	RSSI           flexVal `json:"rssi"`
	Bridge         flexVal `json:"bridge"`
}

// UnmarshalJSON decodes best-effort (see SensorStat).
func (w *WirelessConnectionState) UnmarshalJSON(b []byte) error {
	type alias WirelessConnectionState
	var a alias
	if err := json.Unmarshal(b, &a); err == nil {
		*w = WirelessConnectionState(a)
	}
	return nil
}

// Sensor mirrors the read-side fields the Device Center shows from a
// Protect Integration sensor record. Identity is typed; everything
// else decodes tolerantly and degrades to "-" in the UI.
type Sensor struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"` // "CONNECTED" -> online

	MAC        flexVal `json:"mac"`
	MACAddress flexVal `json:"macAddress"`
	Model      flexVal `json:"model"`
	MarketName flexVal `json:"marketName"`
	Type       flexVal `json:"type"`

	MountType flexVal `json:"mountType"`
	IsOpened  flexVal `json:"isOpened"`

	IsMotionDetected flexVal `json:"isMotionDetected"`
	MotionDetectedAt flexVal `json:"motionDetectedAt"`

	LeakDetectedAt         flexVal `json:"leakDetectedAt"`
	ExternalLeakDetectedAt flexVal `json:"externalLeakDetectedAt"`
	TamperingDetectedAt    flexVal `json:"tamperingDetectedAt"`

	Stats    SensorStats             `json:"stats"`
	Battery  BatteryStatus           `json:"batteryStatus"`
	Wireless WirelessConnectionState `json:"wirelessConnectionState"`

	// Raw is the full decoded object for the detail-panel flatten.
	Raw map[string]any `json:"-"`
}

// DisplayName picks the best human label: name, else the id.
func (s Sensor) DisplayName() string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	return s.ID
}

// IsOnline maps the state string onto online/offline (see Camera).
func (s Sensor) IsOnline() bool {
	return strings.EqualFold(strings.TrimSpace(s.State), "CONNECTED")
}

// MACLabel returns the MAC ("" when absent).
func (s Sensor) MACLabel() string { return firstNonEmpty(s.MAC, s.MACAddress) }

// ModelLabel returns the model name ("" when absent).
func (s Sensor) ModelLabel() string { return firstNonEmpty(s.MarketName, s.Type, s.Model) }

// TemperatureLabel renders stats.temperature.value with its unit
// ("" when absent).
func (s Sensor) TemperatureLabel() string { return labelWithUnit(s.Stats.Temperature.Value, "°C") }

// HumidityLabel renders stats.humidity.value ("" when absent).
func (s Sensor) HumidityLabel() string { return labelWithUnit(s.Stats.Humidity.Value, "%") }

// LightLabel renders stats.light.value ("" when absent).
func (s Sensor) LightLabel() string { return labelWithUnit(s.Stats.Light.Value, "lx") }

// MotionLabel renders the motion state: the isMotionDetected boolean
// when present, otherwise "" (the UI shows "-"). The motionDetectedAt
// timestamp stays visible via the detail-panel flatten.
func (s Sensor) MotionLabel() string { return boolLabel(s.IsMotionDetected) }

// LeakLabel renders the water-leak state from the event timestamps:
// "Yes" while a leak event is fresh, "No" when the timestamps exist
// but are stale, "" when the sensor reports no leak fields at all.
func (s Sensor) LeakLabel(now time.Time) string {
	return recentEventLabel(now, s.LeakDetectedAt, s.ExternalLeakDetectedAt)
}

// TamperLabel renders the tamper state (see LeakLabel).
func (s Sensor) TamperLabel(now time.Time) string {
	return recentEventLabel(now, s.TamperingDetectedAt)
}

// SignalLabel combines signalState and the RSSI when both exist
// ("Good (-62 dBm)"), else whichever is present, else "".
func (s Sensor) SignalLabel() string {
	state := scalarLabel(s.Wireless.SignalState)
	rssi := scalarLabel(s.Wireless.SignalStrength)
	if rssi == "" {
		rssi = scalarLabel(s.Wireless.RSSI)
	}
	switch {
	case state != "" && rssi != "":
		return state + " (" + rssi + " dBm)"
	case state != "":
		return state
	case rssi != "":
		return rssi + " dBm"
	default:
		return ""
	}
}

// BatteryLabel renders the battery percentage, flagged when the NVR
// reports it low ("" when absent).
func (s Sensor) BatteryLabel() string {
	p := scalarLabel(s.Battery.Percentage)
	if p == "" {
		return ""
	}
	if low, ok := s.Battery.IsLow.Bool(); ok && low {
		return p + " % (low)"
	}
	return p + " %"
}

// BridgeLabel returns wirelessConnectionState.bridge ("" when absent).
func (s Sensor) BridgeLabel() string { return scalarLabel(s.Wireless.Bridge) }

// MountTypeLabel returns the mount type ("" when absent).
func (s Sensor) MountTypeLabel() string { return scalarLabel(s.MountType) }

// OpenedLabel renders isOpened as Yes/No ("" when absent).
func (s Sensor) OpenedLabel() string { return boolLabel(s.IsOpened) }

// The value accessors below are the numeric/boolean side of the display
// labels above: they return the raw reading (no unit glued on) plus an
// ok/present flag, for feeding engine readout channels and live cockpit
// gauges. ok is false when the sensor does not report that reading, so a
// bound source can hold its last value rather than snap a Float port to a
// misleading 0.0 (a legitimate temperature) or a Bool port to false.

// TemperatureValue returns stats.temperature.value in °C.
func (s Sensor) TemperatureValue() (float64, bool) { return s.Stats.Temperature.Value.Float() }

// HumidityValue returns stats.humidity.value in %RH.
func (s Sensor) HumidityValue() (float64, bool) { return s.Stats.Humidity.Value.Float() }

// LightValue returns stats.light.value (illuminance) in lux.
func (s Sensor) LightValue() (float64, bool) { return s.Stats.Light.Value.Float() }

// BatteryValue returns the battery percentage (0-100).
func (s Sensor) BatteryValue() (float64, bool) { return s.Battery.Percentage.Float() }

// MotionActive returns the live motion boolean (isMotionDetected).
func (s Sensor) MotionActive() (val, ok bool) { return s.IsMotionDetected.Bool() }

// OpenedActive returns the live door/contact boolean (isOpened).
func (s Sensor) OpenedActive() (val, ok bool) { return s.IsOpened.Bool() }

// LeakActive derives a live leak boolean from the leak event timestamps
// (present is false when the sensor reports no leak fields at all).
func (s Sensor) LeakActive(now time.Time) (active, present bool) {
	return recentEventActive(now, s.LeakDetectedAt, s.ExternalLeakDetectedAt)
}

// TamperActive derives a live tamper boolean from the tamper timestamp.
func (s Sensor) TamperActive(now time.Time) (active, present bool) {
	return recentEventActive(now, s.TamperingDetectedAt)
}

// scalarLabel renders a flexVal like String(), but an object/array (a
// drifted shape) reads as absent - a measurement must show "-" rather
// than raw JSON with a unit glued on.
func scalarLabel(v flexVal) string {
	s := strings.TrimSpace(v.String())
	if s == "" || s[0] == '{' || s[0] == '[' {
		return ""
	}
	return s
}

// labelWithUnit renders a measured value with its display unit, or
// "" when the value is absent or not a scalar.
func labelWithUnit(v flexVal, unit string) string {
	s := scalarLabel(v)
	if s == "" {
		return ""
	}
	return s + " " + unit
}

// recentEventWindow is how long a leak/tamper event timestamp counts
// as "active". The Integration API only reports WHEN the event fired
// (epoch millis), not whether it is still ongoing; ten minutes keeps
// a genuine event visible without pinning a sensor red forever. The
// raw timestamps stay visible in the detail panel.
const recentEventWindow = 10 * time.Minute

// recentEventLabel turns event timestamps into "Yes" (a timestamp is
// within the window - applied on BOTH sides of now, so a small clock
// skew between the NVR and this host cannot suppress a fresh event),
// "No (last <age>)" (timestamps exist but are stale - qualified,
// because an ongoing event whose timestamp never refreshes must not
// read like certainty), or "" (no timestamp fields at all -> "-").
func recentEventLabel(now time.Time, vals ...flexVal) string {
	newest, seen := newestEvent(vals...)
	if !seen {
		return ""
	}
	age := now.Sub(newest)
	if abs := age.Abs(); abs <= recentEventWindow {
		return "Yes"
	}
	if age < 0 {
		age = 0 // far-future timestamp (broken clock): age reads as fresh-ish zero
	}
	return "No (last " + coarseAge(age) + ")"
}

// recentEventActive is the boolean form of recentEventLabel for engine
// readout ports: active is true while the newest timestamp is inside the
// window (symmetric around now, same clock-skew tolerance as the label);
// present is false when the sensor reports no such timestamp at all, so
// the driver can skip the channel rather than push a misleading false.
func recentEventActive(now time.Time, vals ...flexVal) (active, present bool) {
	newest, seen := newestEvent(vals...)
	if !seen {
		return false, false
	}
	return now.Sub(newest).Abs() <= recentEventWindow, true
}

// newestEvent returns the most recent parseable timestamp across vals and
// whether any was present (shared by the label + active accessors).
func newestEvent(vals ...flexVal) (time.Time, bool) {
	var newest time.Time
	seen := false
	for _, v := range vals {
		ms, ok := epochMillis(v)
		if !ok {
			continue
		}
		seen = true
		if at := time.UnixMilli(ms); at.After(newest) {
			newest = at
		}
	}
	return newest, seen
}

// coarseAge renders an event age coarsely for the stale-event label.
func coarseAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + " d ago"
	}
}

// epochMillis reads a flexVal as an epoch-milliseconds timestamp.
// ok is false for absent/zero/non-numeric values.
func epochMillis(v flexVal) (int64, bool) {
	s := strings.TrimSpace(v.String())
	if s == "" {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal([]byte(s), &f); err != nil || f <= 0 {
		return 0, false
	}
	return int64(f), true
}

// ListSensors returns every sensor the NVR reports (see ListCameras
// for the tolerance rules).
func (c *Client) ListSensors(ctx context.Context) ([]Sensor, error) {
	body, err := c.getJSON(ctx, "/v1/sensors")
	if err != nil {
		return nil, err
	}
	items, err := decodeArray(body)
	if err != nil {
		return nil, fmt.Errorf("protectapi: unmarshal sensors: %w", err)
	}
	out := make([]Sensor, 0, len(items))
	for _, item := range items {
		var sen Sensor
		if err := json.Unmarshal(item, &sen); err != nil {
			sen = Sensor{}
		}
		var raw map[string]any
		if json.Unmarshal(item, &raw) == nil {
			sen.Raw = raw
		}
		fillIdentityFromRaw(&sen.ID, &sen.Name, &sen.State, sen.Raw)
		if sen.ID == "" {
			continue
		}
		out = append(out, sen)
	}
	return out, nil
}
