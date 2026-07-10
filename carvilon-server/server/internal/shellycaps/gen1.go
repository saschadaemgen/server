// Gen1 capability table. Gen1 devices identify with a frozen "type" code
// (GET /shelly, mDNS instance prefix) instead of the Gen2 app slug, and
// their channel sets are fixed per code - so unlike the Gen2 substring
// heuristics this is an explicit table of the documented models. Scope
// (Gen1 M1): relay-class, mains-powered devices - Shelly 1 / 1PM / 2 /
// 2.5 (relay mode) / Plug family. Roller mode, dimmers, RGBW and the
// battery/sleepy sensors are deliberate follow-ups, not silently mapped.
package shellycaps

import "strings"

// gen1Model is one Gen1 type-code row.
type gen1Model struct {
	label  string // human name, English UI
	relays int
	meter  bool // true when EVERY relay channel has a real power meter
}

// gen1Table maps the documented Gen1 type codes of the relay-class scope.
// Shelly 1 (SHSW-1) reports a meters[] entry but it is the user power
// CONSTANT, not a measurement (documented) - so meter=false. Shelly 2
// (SHSW-21) has one shared meter for two relays; per-channel metering
// would lie, so it maps as unmetered until a shared-meter surface exists.
var gen1Table = map[string]gen1Model{
	"SHSW-1":   {label: "Shelly 1", relays: 1, meter: false},
	"SHSW-L":   {label: "Shelly 1L", relays: 1, meter: false},
	"SHSW-PM":  {label: "Shelly 1PM", relays: 1, meter: true},
	"SHSW-21":  {label: "Shelly 2", relays: 2, meter: false},
	"SHSW-25":  {label: "Shelly 2.5", relays: 2, meter: true},
	"SHPLG-1":  {label: "Shelly Plug", relays: 1, meter: true},
	"SHPLG2-1": {label: "Shelly Plug E", relays: 1, meter: true},
	"SHPLG-U1": {label: "Shelly Plug US", relays: 1, meter: true},
	"SHPLG-S":  {label: "Shelly Plug S", relays: 1, meter: true},
}

// Gen1Channels returns the relay channels for a Gen1 type code, honouring
// the device mode where it matters: a Shelly 2/2.5 in roller mode has NO
// relay channels (roller control is a follow-up capability, returning
// relays for it would drive the wrong actuator). Unknown codes fall back
// to a single non-metered relay - the same safe minimum as Gen2.
func Gen1Channels(typeCode, mode string) []Channel {
	m, ok := gen1Table[normalizeGen1Type(typeCode)]
	if !ok {
		return []Channel{{ID: 0, Meter: false}}
	}
	if strings.EqualFold(strings.TrimSpace(mode), "roller") && m.relays > 1 {
		return nil
	}
	out := make([]Channel, 0, m.relays)
	for i := 0; i < m.relays; i++ {
		out = append(out, Channel{ID: i, Meter: m.meter})
	}
	return out
}

// Gen1ModelLabel renders a Gen1 type code as its human name ("SHSW-25" ->
// "Shelly 2.5"); unknown codes come back verbatim so the row still shows
// what the device said.
func Gen1ModelLabel(typeCode string) string {
	if m, ok := gen1Table[normalizeGen1Type(typeCode)]; ok {
		return m.label
	}
	return strings.TrimSpace(typeCode)
}

// IsGen1Type reports whether a type code is in the supported Gen1
// relay-class table (the discovery whitelist uses name prefixes, not
// this; this answers "do we know its shape").
func IsGen1Type(typeCode string) bool {
	_, ok := gen1Table[normalizeGen1Type(typeCode)]
	return ok
}

func normalizeGen1Type(typeCode string) string {
	return strings.ToUpper(strings.TrimSpace(typeCode))
}
