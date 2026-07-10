// Gen1 designer/cockpit endpoints - the REST-backed siblings of the Gen2
// HTTP-RPC surface: the overview (channel names + on-device schedule
// state), the device/channel settings surface built from the real
// /settings tree (the same coverage discipline as Gen2: every key
// surfaced, read-only, or deferred with a reason - the list lives in
// docs/shelly-gen1-settings-coverage.md), and the schedule_rules editor.
//
// Two hard rules carried over: the device's MQTT settings are READ-ONLY
// here (provisioning owns them - the Gen2 rule), and every write goes
// through a key whitelist so the panel can never push an arbitrary or
// invented settings key onto the device.
package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"carvilon.local/server/internal/shelly1api"
	"carvilon.local/server/internal/shellycaps"
)

// shelly1Schedule is one channel's on-device schedule state as stored in
// /settings/relay/{ch}: the enable flag plus the raw schedule_rules
// strings ("0700-0123456-on"; weekday digits 0=Monday).
type shelly1Schedule struct {
	Enabled bool     `json:"enabled"`
	Rules   []string `json:"rules"`
}

// writeShelly1Overview serves the faceplate/cockpit load bundle for a
// Gen1 device: per-channel display names and the on-device schedule
// state, from one /settings read. Best-effort like the Gen2 overview: an
// unreachable device yields empty maps rather than an error, so the
// faceplate keeps its CH-N placeholders. The response carries "gen1":
// true plus a "schedules" map instead of Gen2 "jobs" - the two schedule
// models are different on purpose (cron jobs vs schedule_rules strings)
// and pretending otherwise would lie to the editor.
func (s *Server) writeShelly1Overview(w http.ResponseWriter, r *http.Request, id int64) {
	names := map[string]string{}
	schedules := map[string]shelly1Schedule{}
	if cl, err := s.shelly1ClientForID(r.Context(), id); err == nil {
		if sett, serr := cl.GetSettings(r.Context()); serr == nil {
			for i, rl := range sett.Relays {
				key := strconv.Itoa(i)
				if n := strings.TrimSpace(rl.Name.String()); n != "" {
					names[key] = n
				}
				enabled, _ := rl.Schedule.Bool()
				if enabled || len(rl.ScheduleRules) > 0 {
					schedules[key] = shelly1Schedule{Enabled: enabled, Rules: rl.ScheduleRules}
				}
			}
		}
	}
	designerJSON(w, http.StatusOK, map[string]any{
		"gen1":      true,
		"names":     names,
		"jobs":      []any{},
		"schedules": schedules,
	})
}

// ---- M2: the settings surface from the real /settings tree ----

// gen1DeviceKeys whitelists the device-level /settings keys the panel may
// write. Deliberately absent (see the coverage doc): mqtt_* (provisioning
// owns them), login (the fleet auth rotation owns it), timezone/location
// (deferred), coiot (deliberately unused), factory reset (destructive,
// not offered on this surface).
var gen1DeviceKeys = map[string]bool{
	"name": true, "mode": true,
	"led_status_disable": true, "led_power_disable": true,
}

// gen1RelayKeys whitelists the per-channel /settings/relay/{i} keys.
// schedule/schedule_rules go through the dedicated schedule endpoint so
// the whole-set semantics stay in one place.
var gen1RelayKeys = map[string]bool{
	"name": true, "appliance_type": true, "default_state": true,
	"btn_type": true, "btn_reverse": true,
	"auto_on": true, "auto_off": true, "max_power": true,
}

// gen1EnumValues constrains the enum-valued keys so a crafted body cannot
// push an out-of-vocabulary value at the device.
var gen1EnumValues = map[string]map[string]bool{
	"mode":          {"relay": true, "roller": true},
	"default_state": {"off": true, "on": true, "last": true, "switch": true},
	"btn_type": {"momentary": true, "toggle": true, "edge": true,
		"detached": true, "action": true, "momentary_on_release": true},
}

// handleDesignerShelly1Device returns the device-level settings view for
// the cockpit's right column: typed, whitelisted fields only - the raw
// /settings tree is never forwarded (it is foreign input and its shape
// may carry secrets on some firmware). The OTA state rides along
// (best-effort) so the panel shows a pending update without a second
// round trip. Route: GET /a/designer/shelly/{id}/gen1/device.
func (s *Server) handleDesignerShelly1Device(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.shelly1ClientFromPath(w, r)
	if !ok {
		return
	}
	sett, err := cl.GetSettings(r.Context())
	if err != nil {
		http.Error(w, shellyFriendlyError(err), http.StatusBadGateway)
		return
	}
	typeCode := strings.TrimSpace(sett.Device.Type.String())
	view := map[string]any{
		"ok":    true,
		"type":  typeCode,
		"model": shellycaps.Gen1ModelLabel(typeCode),
		"name":  strings.TrimSpace(sett.Name.String()),
		"fw":    strings.TrimSpace(sett.FW.String()),
		"mode":  strings.TrimSpace(sett.Mode.String()),
		"mqtt": map[string]any{
			"enable": flexBool(sett.MQTT.Enable), "server": sett.MQTT.Server.String(),
			"user": sett.MQTT.User.String(), "id": sett.MQTT.ID.String(),
			"retain": flexBool(sett.MQTT.Retain), "max_qos": sett.MQTT.MaxQoS.String(),
			"update_period": sett.MQTT.UpdatePeriod.String(),
		},
		"cloud": map[string]any{"enabled": flexBool(sett.Cloud.Enabled)},
		"login": map[string]any{
			"enabled":  flexBool(sett.Login.Enabled),
			"username": sett.Login.Username.String(),
		},
	}
	// The front-LED switches exist only on the Plug family; presence is
	// keyed on the field being reported at all, not on the model table.
	if !sett.LEDStatusDisable.Empty() || !sett.LEDPowerDisable.Empty() {
		view["led"] = map[string]any{
			"status_disable": flexBool(sett.LEDStatusDisable),
			"power_disable":  flexBool(sett.LEDPowerDisable),
		}
	}
	if ota, oerr := cl.GetOTA(r.Context()); oerr == nil {
		hasUpdate, _ := ota.HasUpdate.Bool()
		view["ota"] = map[string]any{
			"status": ota.Status.String(), "has_update": hasUpdate,
			"old": ota.OldVersion.String(), "new": ota.NewVersion.String(),
		}
	}
	designerJSON(w, http.StatusOK, view)
}

// handleDesignerShelly1DeviceSettings applies whitelisted device-level
// keys. Route: POST /a/designer/shelly/{id}/gen1/device-settings, body
// {"config":{key:value}} (the Gen2 panel envelope).
func (s *Server) handleDesignerShelly1DeviceSettings(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.shelly1ClientFromPath(w, r)
	if !ok {
		return
	}
	params, ok := gen1ConfigParams(w, r, gen1DeviceKeys)
	if !ok {
		return
	}
	if err := cl.SetDeviceSettings(r.Context(), params); err != nil {
		http.Error(w, shellyFriendlyError(err), http.StatusBadGateway)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDesignerShelly1Channel returns one channel's settings + schedule
// for the panel pre-fill. Route: GET /a/designer/shelly/{id}/gen1/channel/{ch}.
func (s *Server) handleDesignerShelly1Channel(w http.ResponseWriter, r *http.Request) {
	cl, ch, ok := s.shelly1ClientChFromPath(w, r)
	if !ok {
		return
	}
	sett, err := cl.GetSettings(r.Context())
	if err != nil {
		http.Error(w, shellyFriendlyError(err), http.StatusBadGateway)
		return
	}
	if ch >= len(sett.Relays) {
		http.Error(w, "no such channel", http.StatusNotFound)
		return
	}
	rl := sett.Relays[ch]
	typeCode := strings.TrimSpace(sett.Device.Type.String())
	chans := shellycaps.Gen1Channels(typeCode, strings.TrimSpace(sett.Mode.String()))
	enabled, _ := rl.Schedule.Bool()
	designerJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"relay": map[string]any{
			"name":           rl.Name.String(),
			"appliance_type": rl.ApplianceType.String(),
			"default_state":  rl.DefaultState.String(),
			"btn_type":       rl.BtnType.String(),
			"btn_reverse":    flexBool(rl.BtnReversed),
			"auto_on":        rl.AutoOn.String(),
			"auto_off":       rl.AutoOff.String(),
			"max_power":      rl.MaxPower.String(),
		},
		"meter":    ch < len(chans) && chans[ch].Meter,
		"schedule": shelly1Schedule{Enabled: enabled, Rules: rl.ScheduleRules},
	})
}

// handleDesignerShelly1ChannelSettings applies whitelisted per-channel
// keys. Route: POST /a/designer/shelly/{id}/gen1/channel/{ch}/settings.
func (s *Server) handleDesignerShelly1ChannelSettings(w http.ResponseWriter, r *http.Request) {
	cl, ch, ok := s.shelly1ClientChFromPath(w, r)
	if !ok {
		return
	}
	params, ok := gen1ConfigParams(w, r, gen1RelayKeys)
	if !ok {
		return
	}
	if err := cl.SetRelaySettings(r.Context(), ch, params); err != nil {
		http.Error(w, shellyFriendlyError(err), http.StatusBadGateway)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// gen1MaxScheduleRules caps a written rule set. The relay-class models
// accept 18 (Shelly 1/1PM, per the official doc) to 20 (2/2.5/Plug, also
// the on-device UI's cap); the stricter bound keeps a set valid on every
// scoped model.
const gen1MaxScheduleRules = 18

// gen1FixedRule is the documented fixed-time rule: HHMM (24h, leading
// zeros) - weekday digits (0=Monday..6=Sunday) - on/off.
var gen1FixedRule = regexp.MustCompile(`^([01][0-9]|2[0-3])[0-5][0-9]-[0-6]{1,7}-(on|off)$`)

// gen1SunRule is the sunrise/sunset variant: the HHMM field carries the
// zero-padded OFFSET MAGNITUDE and a 3-letter modifier carries the sign +
// event - asr/bsr = after/before sunrise, ass/bss = after/before sunset
// ("0030asr-0123456-off" = sunrise+30min; "0000ass" = at sunset). The
// token is NOT in the official (frozen) API doc - it is reverse-
// documented from the on-device web UI source and device-returned
// /settings dumps (consistent across sources) and stays VERIFICATION-
// PENDING until a real-device round-trip confirms it. Writes stay inside
// the stock UI's guaranteed offset range (<= 02:59); the strict pattern
// means we only ever write shapes we also parse back.
var gen1SunRule = regexp.MustCompile(`^0[0-2][0-5][0-9](asr|bsr|ass|bss)-[0-6]{1,7}-(on|off)$`)

// validGen1ScheduleRule reports whether one rule string is a shape we
// know how to write and read back.
func validGen1ScheduleRule(rule string) bool {
	return gen1FixedRule.MatchString(rule) || gen1SunRule.MatchString(rule)
}

// handleDesignerShelly1Schedule replaces one channel's on-device schedule
// as a whole set (the documented Gen1 semantics - no append). Route:
// POST /a/designer/shelly/{id}/gen1/channel/{ch}/schedule, body
// {"enabled":bool,"rules":["0700-0123456-on",...]}.
func (s *Server) handleDesignerShelly1Schedule(w http.ResponseWriter, r *http.Request) {
	cl, ch, ok := s.shelly1ClientChFromPath(w, r)
	if !ok {
		return
	}
	var in struct {
		Enabled bool     `json:"enabled"`
		Rules   []string `json:"rules"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if len(in.Rules) > gen1MaxScheduleRules {
		http.Error(w, fmt.Sprintf("at most %d schedule rules", gen1MaxScheduleRules), http.StatusBadRequest)
		return
	}
	for _, rule := range in.Rules {
		if !validGen1ScheduleRule(rule) {
			http.Error(w, "invalid schedule rule", http.StatusBadRequest)
			return
		}
	}
	if err := cl.SetScheduleRules(r.Context(), ch, in.Enabled, in.Rules); err != nil {
		http.Error(w, shellyFriendlyError(err), http.StatusBadGateway)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDesignerShelly1Reboot restarts the device (settings like MQTT
// only apply after one). Route: POST /a/designer/shelly/{id}/gen1/reboot.
func (s *Server) handleDesignerShelly1Reboot(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.shelly1ClientFromPath(w, r)
	if !ok {
		return
	}
	if err := cl.Reboot(r.Context()); err != nil {
		http.Error(w, shellyFriendlyError(err), http.StatusBadGateway)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDesignerShelly1OTAUpdate starts a firmware update to the latest
// release. Route: POST /a/designer/shelly/{id}/gen1/ota-update.
func (s *Server) handleDesignerShelly1OTAUpdate(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.shelly1ClientFromPath(w, r)
	if !ok {
		return
	}
	if err := cl.TriggerOTAUpdate(r.Context()); err != nil {
		http.Error(w, shellyFriendlyError(err), http.StatusBadGateway)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- shared plumbing ----

// shelly1ClientFromPath resolves {id} to a Gen1 client, writing the error
// response itself (ok=false means "already answered").
func (s *Server) shelly1ClientFromPath(w http.ResponseWriter, r *http.Request) (*shelly1api.Client, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return nil, false
	}
	cl, err := s.shelly1ClientForID(r.Context(), id)
	if err != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return nil, false
	}
	return cl, true
}

// shelly1ClientChFromPath additionally parses the {ch} channel.
func (s *Server) shelly1ClientChFromPath(w http.ResponseWriter, r *http.Request) (*shelly1api.Client, int, bool) {
	id, ch, ok := shellyIDCh(r)
	if !ok {
		http.Error(w, "bad id or channel", http.StatusBadRequest)
		return nil, 0, false
	}
	cl, err := s.shelly1ClientForID(r.Context(), id)
	if err != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return nil, 0, false
	}
	return cl, ch, true
}

// gen1ConfigParams decodes the {"config":{key:value}} panel envelope into
// whitelisted, enum-validated query params. Booleans render as 1/0 (the
// documented form), numbers as plain decimals.
func gen1ConfigParams(w http.ResponseWriter, r *http.Request, allowed map[string]bool) (url.Values, bool) {
	var in struct {
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil || len(in.Config) == 0 {
		http.Error(w, "bad body", http.StatusBadRequest)
		return nil, false
	}
	params := url.Values{}
	for k, v := range in.Config {
		if !allowed[k] {
			continue // dropped, never forwarded (the whitelist rule)
		}
		val, err := gen1ParamValue(k, v)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return nil, false
		}
		params.Set(k, val)
	}
	if len(params) == 0 {
		http.Error(w, "no supported settings key in body", http.StatusBadRequest)
		return nil, false
	}
	return params, true
}

// gen1ParamValue renders one JSON value as its query-parameter form,
// enforcing the enum vocabularies.
func gen1ParamValue(key string, v any) (string, error) {
	var out string
	switch t := v.(type) {
	case bool:
		if t {
			out = "1"
		} else {
			out = "0"
		}
	case float64:
		out = strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		out = strings.TrimSpace(t)
	default:
		return "", errors.New("unsupported value type")
	}
	if enum, ok := gen1EnumValues[key]; ok && !enum[out] {
		return "", errors.New("invalid value for " + key)
	}
	return out, nil
}

// flexBool folds a tolerant boolean field to a plain bool (false when
// absent/unrecognisable - the panel then shows the toggle off).
func flexBool(f interface{ Bool() (bool, bool) }) bool {
	v, _ := f.Bool()
	return v
}
