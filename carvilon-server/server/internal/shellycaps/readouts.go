package shellycaps

import (
	"strconv"
	"strings"
)

// Readout is one metered value a Shelly channel reports on the broker: a
// stable metric token, its display label and unit. It is the capability
// declaration the sensor-history path records against and the charts label
// from - the Shelly equivalent of protectmonitor.Readout, derived from the
// model rather than from a live probe so it is available offline.
//
// The tokens are stable identifiers: they key sensor_samples rows, so
// renaming one orphans a device's recorded history.
type Readout struct {
	Token string
	Label string
	Unit  string
}

// Metric token suffixes. Power is the only one every metered generation
// reports; the rest are Gen2+ (see Readouts).
const (
	mPower   = "power"
	mVoltage = "voltage"
	mCurrent = "current"
	mFreq    = "freq"
	mTemp    = "temp"
)

// SwitchReadoutToken builds the metric token for a relay channel's metric.
// Exported so the recorder tap and the capability list cannot drift apart.
func SwitchReadoutToken(ch int, metric string) string {
	return "sw" + strconv.Itoa(ch) + "_" + metric
}

// LightReadoutToken builds the metric token for a light channel's metric.
func LightReadoutToken(ch int, metric string) string {
	return "li" + strconv.Itoa(ch) + "_" + metric
}

// Readouts returns the recordable metrics of a device, per channel, for the
// generation's publish grammar. It follows what the device actually reports,
// so a metric is not declared where the generation cannot send it:
//
//   - Gen2+ metered relay: the status/switch:N payload carries apower,
//     voltage, current, freq and temperature.tC - all five.
//   - Gen1 metered relay: only relay/N/power exists (a bare number). Gen1 has
//     no combined status payload, so voltage/current/frequency are NOT
//     reported and are not declared.
//   - Gen1 light (RGBW2): the color|white/N/status payload carries power. With
//     an unknown mode both shapes are declared - see gen1LightsForRecording.
//   - A non-metered relay reports nothing to record.
//
// This is a declaration, not a gate: the tap records whatever a device
// publishes, and this only supplies the label and unit the charts show. The
// derivation is a heuristic over a model string, so it can be wrong in either
// direction, and no caller may treat "declares nothing" as "records nothing".
//
// typeCode/model is whatever the device set stored; mode is the Gen1 device
// mode ("color"/"white"/""), matching Gen1Lights.
func Readouts(model string, gen1 bool, mode string) []Readout {
	var out []Readout
	if !gen1 {
		for _, c := range Channels(model) {
			if !c.Meter {
				continue
			}
			out = append(out, switchReadouts(c.ID, true)...)
		}
		return out
	}
	for _, c := range Gen1Channels(model, mode) {
		if !c.Meter {
			continue
		}
		out = append(out, switchReadouts(c.ID, false)...)
	}
	for _, l := range gen1LightsForRecording(model, mode) {
		out = append(out, Readout{
			Token: LightReadoutToken(l.ID, mPower),
			Label: "Light " + strconv.Itoa(l.ID+1) + " power",
			Unit:  "W",
		})
	}
	return out
}

// gen1LightsForRecording lists the light channels to DECLARE. With a known
// mode it is exactly Gen1Lights. With an unknown mode ("") it is the UNION of
// the modes, because the device set does not persist the mode: an RGBW2 in
// white mode publishes white/0..3/status and the tap records li0..li3_power,
// and declaring only the color shape (li0) would leave three real, recorded
// metrics with no label and no unit.
//
// Over-declaring is the safe direction here: a metric that never records
// simply has no samples, and only recorded metrics are charted.
func gen1LightsForRecording(model, mode string) []Light {
	if strings.TrimSpace(mode) != "" {
		return Gen1Lights(model, mode)
	}
	union := Gen1Lights(model, "color")
	seen := map[int]bool{}
	for _, l := range union {
		seen[l.ID] = true
	}
	for _, l := range Gen1Lights(model, "white") {
		if !seen[l.ID] {
			union = append(union, l)
		}
	}
	return union
}

// switchReadouts lists one relay channel's metrics. full=false is the Gen1
// shape (power only).
func switchReadouts(ch int, full bool) []Readout {
	name := "CH" + strconv.Itoa(ch+1) + " "
	out := []Readout{
		{Token: SwitchReadoutToken(ch, mPower), Label: name + "power", Unit: "W"},
	}
	if !full {
		return out
	}
	return append(out,
		Readout{Token: SwitchReadoutToken(ch, mVoltage), Label: name + "voltage", Unit: "V"},
		Readout{Token: SwitchReadoutToken(ch, mCurrent), Label: name + "current", Unit: "A"},
		Readout{Token: SwitchReadoutToken(ch, mFreq), Label: name + "frequency", Unit: "Hz"},
		Readout{Token: SwitchReadoutToken(ch, mTemp), Label: name + "temperature", Unit: "°C"},
	)
}
