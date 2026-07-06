// Shelly.GetStatus + Shelly.GetConfig - Saison 21, Shelly Etappe 1.
// GetStatus returns the state of ALL components in one call; the
// Device Center panel shows the switch channels (power, voltage,
// current, frequency, energy, on/off) and the inputs. GetConfig is
// only read for the component names (channel labels like "SANlight
// One") - names live in the config, not in the status.
//
// The decode is deliberately forgiving: components are picked out of
// the result object by their "switch:N" / "input:N" keys, every field
// inside is a flexVal, and a component that is not even an object
// just renders as a channel with "-" values. Exact field names were
// taken from the Gen2 documentation and must survive firmware drift,
// so nothing here is typed harder than necessary.
package shellyapi

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// SwitchStatus is one "switch:N" component from Shelly.GetStatus.
type SwitchStatus struct {
	ID          int
	Output      flexVal // on/off
	APower      flexVal // active power, W
	Voltage     flexVal // V
	Current     flexVal // A
	Freq        flexVal // Hz
	EnergyTotal flexVal // aenergy.total, Wh
}

// InputStatus is one "input:N" component from Shelly.GetStatus.
type InputStatus struct {
	ID      int
	State   flexVal // bool for switch-type inputs, null when disabled
	Percent flexVal // analog inputs report a percentage instead
}

// Status carries the switch and input components of one device,
// sorted by component id.
type Status struct {
	Switches []SwitchStatus
	Inputs   []InputStatus
}

// GetStatus fetches the live component status (one call for all
// components - the briefed read path).
func (c *Client) GetStatus(ctx context.Context) (*Status, error) {
	result, err := c.call(ctx, "Shelly.GetStatus")
	if err != nil {
		return nil, err
	}
	var components map[string]json.RawMessage
	if err := json.Unmarshal(result, &components); err != nil {
		// Fixed text, not the json error: every error leaving this
		// package must stay redacted (callers log them verbatim).
		return nil, errors.New("shellyapi: status payload is not a component map")
	}
	st := &Status{}
	for key, raw := range components {
		kind, id, ok := splitComponentKey(key)
		if !ok {
			continue
		}
		fields := decodeComponent(raw)
		switch kind {
		case "switch":
			sw := SwitchStatus{
				ID:      id,
				Output:  fields["output"],
				APower:  fields["apower"],
				Voltage: fields["voltage"],
				Current: fields["current"],
				Freq:    fields["freq"],
			}
			// aenergy is a nested object {"total": ..., ...}; a
			// non-object value simply leaves the total absent.
			sw.EnergyTotal = decodeComponent(fields["aenergy"].raw)["total"]
			st.Switches = append(st.Switches, sw)
		case "input":
			st.Inputs = append(st.Inputs, InputStatus{
				ID:      id,
				State:   fields["state"],
				Percent: fields["percent"],
			})
		}
	}
	sort.Slice(st.Switches, func(i, j int) bool { return st.Switches[i].ID < st.Switches[j].ID })
	sort.Slice(st.Inputs, func(i, j int) bool { return st.Inputs[i].ID < st.Inputs[j].ID })
	return st, nil
}

// Config carries the one thing the Device Center reads from
// Shelly.GetConfig: the user-set component names, keyed by component
// ("switch:0" -> "SANlight One"). Unnamed components are absent.
type Config struct {
	Names map[string]string
}

// GetConfig fetches the device configuration and extracts the
// component names.
func (c *Client) GetConfig(ctx context.Context) (*Config, error) {
	result, err := c.call(ctx, "Shelly.GetConfig")
	if err != nil {
		return nil, err
	}
	var components map[string]json.RawMessage
	if err := json.Unmarshal(result, &components); err != nil {
		return nil, errors.New("shellyapi: config payload is not a component map")
	}
	cfg := &Config{Names: map[string]string{}}
	for key, raw := range components {
		if name := strings.TrimSpace(decodeComponent(raw)["name"].String()); name != "" {
			cfg.Names[key] = name
		}
	}
	return cfg, nil
}

// SwitchName returns the configured name of switch channel id, "" when
// unnamed. Nil-safe so callers can chain a failed config fetch.
func (c *Config) SwitchName(id int) string {
	if c == nil {
		return ""
	}
	return c.Names["switch:"+strconv.Itoa(id)]
}

// InputName returns the configured name of input channel id, "" when
// unnamed. Nil-safe.
func (c *Config) InputName(id int) string {
	if c == nil {
		return ""
	}
	return c.Names["input:"+strconv.Itoa(id)]
}

// splitComponentKey splits "switch:0" into ("switch", 0, true).
// Component keys without a numeric instance ("sys", "wifi") report
// ok=false - the Device Center only reads instanced components. The
// id must be CANONICAL decimal ("0", not "00" or "+0") and small:
// non-canonical spellings are distinct JSON keys that would otherwise
// smuggle duplicate channel ids (or an absurd id) into the panel.
func splitComponentKey(key string) (kind string, id int, ok bool) {
	i := strings.IndexByte(key, ':')
	if i < 0 {
		return "", 0, false
	}
	suffix := key[i+1:]
	id, err := strconv.Atoi(suffix)
	if err != nil || id < 0 || id > 9999 || strconv.Itoa(id) != suffix {
		return "", 0, false
	}
	return key[:i], id, true
}

// decodeComponent decodes one component object into its fields.
// Tolerant by construction: a value that is not an object (or nil raw
// bytes) yields an empty map, and flexVal fields never fail - so a
// single drifted component can never kill the whole status.
func decodeComponent(raw json.RawMessage) map[string]flexVal {
	fields := map[string]flexVal{}
	if len(raw) == 0 {
		return fields
	}
	_ = json.Unmarshal(raw, &fields)
	return fields
}

// StateLabel renders the switch output as "On"/"Off"; a value that is
// not a recognisable boolean renders verbatim, absent renders "".
func (s SwitchStatus) StateLabel() string { return onOffLabel(s.Output) }

// PowerLabel returns the active power with its unit ("" when absent).
func (s SwitchStatus) PowerLabel() string { return unitLabel(s.APower, "W") }

// VoltageLabel returns the voltage with its unit ("" when absent).
func (s SwitchStatus) VoltageLabel() string { return unitLabel(s.Voltage, "V") }

// CurrentLabel returns the current with its unit ("" when absent).
func (s SwitchStatus) CurrentLabel() string { return unitLabel(s.Current, "A") }

// FreqLabel returns the mains frequency with its unit ("" when absent).
func (s SwitchStatus) FreqLabel() string { return unitLabel(s.Freq, "Hz") }

// EnergyLabel returns the accumulated energy with its unit ("" when
// absent).
func (s SwitchStatus) EnergyLabel() string { return unitLabel(s.EnergyTotal, "Wh") }

// StateLabel renders a switch-type input as "On"/"Off"; analog inputs
// report their percentage instead; absent/disabled renders "".
func (i InputStatus) StateLabel() string {
	if !i.State.Empty() {
		return onOffLabel(i.State)
	}
	return unitLabel(i.Percent, "%")
}

// onOffLabel maps a boolean-ish value to On/Off, keeping anything
// unrecognisable verbatim (tolerant display, nothing invented).
func onOffLabel(v flexVal) string {
	if b, ok := v.Bool(); ok {
		if b {
			return "On"
		}
		return "Off"
	}
	return strings.TrimSpace(v.String())
}

// unitLabel renders a measurement with its unit, "" when absent (the
// panel then shows "-"). Non-numeric values are shown verbatim with
// the unit - the honest reading of a drifted field.
func unitLabel(v flexVal, unit string) string {
	s := strings.TrimSpace(v.String())
	if s == "" {
		return ""
	}
	return s + " " + unit
}
