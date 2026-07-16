// Package shellycaps derives a Shelly device's controllable channel set
// from what CARVILON knows about it. M1 derives the metered-relay count
// from the device model string (offline, deterministic) so the Logic
// Editor's Shelly module is capability-aware per model rather than
// hard-coding a fixed channel count. A later step refines this from the
// device's live status (the real switch:N components on the broker), for
// which this stays the offline fallback.
package shellycaps

import "strings"

// Channel is one controllable relay channel of a Shelly device.
type Channel struct {
	ID    int  // Gen2 component index (switch:ID)
	Meter bool // true when the channel reports power/energy (…PM models)
}

// Channels returns the relay channels for a device model, ordered by id.
// The model string is whatever the device reported (e.g. "Shelly Pro4PM"
// or a raw code); matching is case-insensitive and tolerant. An unknown
// model falls back to a single, non-metered relay - the safe minimum for
// a switch device - so a never-classified Shelly still yields a usable
// (if conservative) module.
func Channels(model string) []Channel {
	n := relayCount(model)
	m := strings.ToUpper(model)
	// Metered when the model marks it: "PM" (app slug, e.g. Pro4PM) or the
	// "…PE…" Pro-switch raw code (SPSW-x0YPE16EU). A non-metered relay
	// (plain switch) reports no power - the faceplate then hides the meter.
	//
	// "PLUG" is metered too and carries no such marker: a Plus Plug S reports
	// as "Shelly PlusPlugS" and matches none of the above, so it read as a
	// plain switch and its power/voltage/current went unmodelled - the Gen1
	// table has always marked every SHPLG-* plug metered, and the Gen2 plugs
	// meter the same way.
	meter := strings.Contains(m, "PM") || strings.Contains(m, "PE") ||
		strings.Contains(m, "EM") || strings.Contains(m, "PLUG")
	out := make([]Channel, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Channel{ID: i, Meter: meter})
	}
	return out
}

// relayCount maps a model string to its number of relay channels. It
// matches both the human/app model ("Shelly Pro4PM", "Plus1PM" - what the
// device set usually stores, from the mDNS "app" TXT) AND the raw Gen2
// switch code ("SPSW-x0YPE16EU", where "0Y" before "PE" is the channel
// count). An unrecognised model falls back to a single relay - the safe
// minimum a live switch:N enumeration refines later.
func relayCount(model string) int {
	m := strings.ToUpper(model)
	switch {
	case strings.Contains(m, "4PM"), strings.Contains(m, "PRO4"), strings.Contains(m, "04PE"):
		return 4
	case strings.Contains(m, "3EM"), strings.Contains(m, "3PM"), strings.Contains(m, "03PE"):
		return 3
	case strings.Contains(m, "2PM"), strings.Contains(m, "PRO2"),
		strings.Contains(m, "PLUS2"), strings.Contains(m, "PLUS 2"),
		strings.Contains(m, "02PE"):
		return 2
	case strings.Contains(m, "1PM"), strings.Contains(m, "1PN"),
		strings.Contains(m, "PRO1"), strings.Contains(m, "PLUS1"),
		strings.Contains(m, "PLUS 1"), strings.Contains(m, "01PE"):
		return 1
	default:
		return 1 // safe minimum: a plain switch device has one relay
	}
}
