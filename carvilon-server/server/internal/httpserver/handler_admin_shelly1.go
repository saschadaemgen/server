// Gen1 Device Center surface - the slide-out detail for a Gen1 row. The
// Gen2 sibling reads Shelly.GetStatus/GetConfig over JSON-RPC; here the
// same panel truth comes from the frozen REST endpoints: GET /status
// (relays[]/meters[]/inputs[] arrays, index = channel) with the channel
// names and the capability-deciding type/mode from GET /settings.
//
// Honesty rules carried over from the Gen2 panel plus two Gen1-specific
// ones: a Shelly 1 reports a meters[] entry that is a user CONSTANT, not
// a measurement (the capability table, not the meter's presence, decides
// whether power renders), and energy counters are watt-minutes that reset
// on reboot (rendered as Wh with the caveat living in the docs).
package httpserver

import (
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/shelly1api"
	"carvilon.local/server/internal/shellycaps"
)

// writeShelly1Detail serves the lazy panel sections for one Gen1 device.
func (s *Server) writeShelly1Detail(w http.ResponseWriter, r *http.Request, client *shelly1api.Client) {
	st, err := client.GetStatus(r.Context())
	if err != nil {
		s.log.Warn("device center: shelly gen1 status failed", "err", err)
		writeUADetail(w, uaSection{Title: "Relays", Error: shellyFriendlyError(err)})
		return
	}
	// Names and the capability shape are cosmetic here: a failed settings
	// read only drops the labels and the meter columns (conservative -
	// without the type code we do not guess which meters are real).
	sett, serr := client.GetSettings(r.Context())
	if serr != nil {
		s.log.Warn("device center: shelly gen1 settings failed", "err", serr)
		sett = nil
	}
	var chans []shellycaps.Channel
	mode := ""
	if sett != nil {
		mode = strings.TrimSpace(sett.Mode.String())
		chans = shellycaps.Gen1Channels(strings.TrimSpace(sett.Device.Type.String()), mode)
	}
	metered := func(i int) bool { return i < len(chans) && chans[i].Meter }
	relayName := func(i int) string {
		if sett != nil && i < len(sett.Relays) {
			return strings.TrimSpace(sett.Relays[i].Name.String())
		}
		return ""
	}

	sections := make([]uaSection, 0, len(st.Relays)+2)
	for i, rl := range st.Relays {
		// 1-based titles match the terminal print on the device; the
		// 0-based channel ids stay an internal detail (the Gen2 pattern).
		title := "Relay " + strconv.Itoa(i+1)
		if name := relayName(i); name != "" {
			title += " · " + name
		}
		sec := uaSection{Title: title}
		sec.Rows = appendKVDash(sec.Rows, "State", rl.StateLabel())
		if metered(i) && i < len(st.Meters) {
			sec.Rows = appendKVDash(sec.Rows, "Power", st.Meters[i].PowerLabel())
			sec.Rows = appendKVDash(sec.Rows, "Energy", st.Meters[i].EnergyLabel())
		}
		sections = append(sections, sec)
	}
	if len(st.Relays) == 0 && strings.EqualFold(mode, "roller") {
		// A 2.5 in roller mode has no relay channels; say so instead of
		// rendering an empty panel (roller control is a follow-up).
		sections = append(sections, uaSection{Title: "Roller", Rows: []kvRow{
			{Key: "Mode", Value: "Roller (not yet supported - relay mode only)"},
		}})
	}
	if len(st.Inputs) > 0 {
		sec := uaSection{Title: "Inputs"}
		for i, in := range st.Inputs {
			state := "-"
			if v, ok := in.Input.Bool(); ok {
				if v {
					state = "On"
				} else {
					state = "Off"
				}
			}
			sec.Rows = appendKVDash(sec.Rows, "Input "+strconv.Itoa(i+1), state)
		}
		sections = append(sections, sec)
	}
	dev := uaSection{Title: "Device"}
	dev.Rows = appendKV(dev.Rows, "Temperature", st.TempLabel())
	if v, ok := st.MQTT.Connected.Bool(); ok {
		label := "Not connected"
		if v {
			label = "Connected"
		}
		dev.Rows = appendKV(dev.Rows, "MQTT (device view)", label)
	}
	if len(dev.Rows) > 0 {
		sections = append(sections, dev)
	}
	writeUADetail(w, sections...)
}
