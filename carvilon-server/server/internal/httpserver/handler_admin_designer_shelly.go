package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/shellyapi"
)

// handleDesignerShellySwitch switches a Shelly relay directly from the
// editor faceplate's clickable toggle, independent of any engine run: it
// publishes a Gen2 JSON-RPC Switch.Set to the device's "<prefix>/rpc"
// topic over the broker's in-process inline client (the same envelope the
// mqtt: driver's RPC sink builds - not a guess). It is the manual half of
// the module's relay control; a graph-driven relay uses the run path and
// the faceplate switch is inert for it (output exclusivity, enforced in
// the editor).
//
// Route: POST /a/designer/shelly/switch (requireAdminSession). Body:
// {"prefix":"carvilon/shelly-<mac>","channel":N,"on":bool}. The prefix
// must be under the Shelly subtree so this endpoint can never be used to
// publish to an arbitrary broker topic.
func (s *Server) handleDesignerShellySwitch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Prefix  string `json:"prefix"`
		Channel int    `json:"channel"`
		On      bool   `json:"on"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	prefix := strings.TrimSpace(in.Prefix)
	// Only the Shelly device subtree, and no wildcards/relative segments,
	// so a crafted prefix cannot address another device account or escape
	// the namespace.
	if !strings.HasPrefix(prefix, "carvilon/shelly-") || strings.ContainsAny(prefix, "#+ ") || in.Channel < 0 || in.Channel > 99 {
		http.Error(w, "invalid prefix or channel", http.StatusBadRequest)
		return
	}
	client, ok := s.mqttInline()
	if !ok {
		http.Error(w, "broker is not running", http.StatusServiceUnavailable)
		return
	}
	// Gen2 JSON-RPC Switch.Set envelope (id fixed: the device confirms the
	// new state on its status topic, which the faceplate reads back; we do
	// not correlate this reply). params carries the relay index and state.
	env := map[string]any{
		"id":     0,
		"src":    prefix + "/rpc/resp",
		"method": "Switch.Set",
		"params": map[string]any{"id": in.Channel, "on": in.On},
	}
	payload, err := json.Marshal(env)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	if err := client.Publish(prefix+"/rpc", payload, false, 0); err != nil {
		s.engineLog.Warn("shelly manual switch publish failed", "component", "shelly", "err", err)
		http.Error(w, "publish failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// shellyClientForID builds a Gen2 HTTP-RPC client for an adopted device by
// its store id, dialling its LAN address with the shared digest password
// (the config/schedule transport - MQTT stays for switching/readouts). The
// address goes back through the LAN guard (defence in depth against a
// poisoned shelly_devices row).
func (s *Server) shellyClientForID(ctx context.Context, id int64) (*shellyapi.Client, error) {
	if s.shellystore == nil {
		return nil, errors.New("shelly store not configured")
	}
	d, err := s.shellystore.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	norm, ok := normalizeShellyAddr(d.Address)
	if !ok || norm == "" {
		return nil, errors.New("device address unavailable")
	}
	var password string
	if s.platformCfg != nil {
		password, _ = s.platformCfg.GetSecret(ctx, platformconfig.KeyShellyPassword)
	}
	return shellyapi.New(shellyapi.Options{Address: norm, Password: password}), nil
}

// shellyIDCh parses the {id} device and {ch} channel path params, bounding
// the channel so a crafted path cannot address an absurd component id.
func shellyIDCh(r *http.Request) (id int64, ch int, ok bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, 0, false
	}
	c, err := strconv.Atoi(r.PathValue("ch"))
	if err != nil || c < 0 || c > 99 {
		return 0, 0, false
	}
	return id, c, true
}

// switchConfigKeys / inputConfigKeys whitelist the config keys the editor
// may write (confirmed on the Pro 4PM). A key outside the set is dropped
// rather than forwarded, so the settings panel can never push an arbitrary
// or invented config field onto the device.
var switchConfigKeys = map[string]bool{
	"name": true, "in_mode": true, "in_locked": true, "initial_state": true,
	"auto_on": true, "auto_on_delay": true, "auto_off": true, "auto_off_delay": true,
	"power_limit": true, "voltage_limit": true, "undervoltage_limit": true,
	"autorecover_voltage_errors": true, "current_limit": true, "reverse": true,
}

var inputConfigKeys = map[string]bool{
	"name": true, "type": true, "enable": true, "invert": true,
}

// handleDesignerShellyOverview returns the per-channel display names
// (Switch.GetConfig.name via Shelly.GetConfig) and the set of channels that
// have an on-board weekly schedule (Schedule.List) - the two things the
// faceplate needs from the device on load, in one round trip. Both parts
// are best-effort: an unreachable device yields empty maps rather than an
// error, so the faceplate keeps its CH-N placeholders.
// Route: GET /a/designer/shelly/{id}/overview.
func (s *Server) handleDesignerShellyOverview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	cl, err := s.shellyClientForID(r.Context(), id)
	if err != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	names := map[string]string{}
	if cfg, err := cl.GetConfig(r.Context()); err == nil && cfg != nil {
		for key, name := range cfg.Names {
			if n, ok := strings.CutPrefix(key, "switch:"); ok && name != "" {
				names[n] = name
			}
		}
	}
	// The full schedule jobs, so the faceplate can both light the clock icon
	// (any job targeting a channel) and show the next scheduled action.
	jobs := []shellyapi.ScheduleJob{}
	if res, err := cl.ScheduleList(r.Context()); err == nil && res != nil {
		jobs = res.Jobs
	}
	designerJSON(w, http.StatusOK, map[string]any{"names": names, "jobs": jobs})
}

// handleDesignerShellyChannel returns a channel's live switch + input config
// (Switch.GetConfig + Input.GetConfig over HTTP-RPC) so the editor settings
// panel pre-fills from the device's real current values, never blank fields.
// Route: GET /a/designer/shelly/{id}/channel/{ch}.
func (s *Server) handleDesignerShellyChannel(w http.ResponseWriter, r *http.Request) {
	id, ch, ok := shellyIDCh(r)
	if !ok {
		http.Error(w, "bad id or channel", http.StatusBadRequest)
		return
	}
	cl, err := s.shellyClientForID(r.Context(), id)
	if err != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	sw, err := cl.SwitchGetConfig(r.Context(), ch)
	if err != nil {
		http.Error(w, "switch config read failed", http.StatusBadGateway)
		return
	}
	// Input config is best-effort: not every metered switch has a matching
	// input, so a failure leaves it null rather than failing the whole read.
	in, _ := cl.InputGetConfig(r.Context(), ch)
	resp := map[string]any{"switch": sw, "input": nil}
	if len(in) > 0 {
		resp["input"] = in
	}
	designerJSON(w, http.StatusOK, resp)
}

// handleDesignerShellySwitchConfig applies a partial channel config over
// HTTP-RPC (Switch.SetConfig). Only whitelisted keys are forwarded.
// Route: POST /a/designer/shelly/{id}/channel/{ch}/switch-config.
func (s *Server) handleDesignerShellySwitchConfig(w http.ResponseWriter, r *http.Request) {
	s.shellySetConfig(w, r, "switch", switchConfigKeys)
}

// handleDesignerShellyInputConfig applies a partial input config over
// HTTP-RPC (Input.SetConfig). Only whitelisted keys are forwarded.
// Route: POST /a/designer/shelly/{id}/channel/{ch}/input-config.
func (s *Server) handleDesignerShellyInputConfig(w http.ResponseWriter, r *http.Request) {
	s.shellySetConfig(w, r, "input", inputConfigKeys)
}

func (s *Server) shellySetConfig(w http.ResponseWriter, r *http.Request, comp string, allowed map[string]bool) {
	id, ch, ok := shellyIDCh(r)
	if !ok {
		http.Error(w, "bad id or channel", http.StatusBadRequest)
		return
	}
	var body struct {
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	cfg := map[string]any{}
	for k, v := range body.Config {
		if allowed[k] {
			cfg[k] = v
		}
	}
	if len(cfg) == 0 {
		http.Error(w, "no writable config keys", http.StatusBadRequest)
		return
	}
	cl, err := s.shellyClientForID(r.Context(), id)
	if err != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	if comp == "input" {
		_, err = cl.InputSetConfig(r.Context(), ch, cfg)
	} else {
		_, err = cl.SwitchSetConfig(r.Context(), ch, cfg)
	}
	if err != nil {
		s.engineLog.Warn("shelly set config failed", "component", "shelly", "err", err)
		http.Error(w, "config write failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDesignerShellySchedules returns the device's on-board schedule jobs
// (Schedule.List). The editor groups on/off pairs per channel and lights the
// clock icon on any channel a job targets.
// Route: GET /a/designer/shelly/{id}/schedules.
func (s *Server) handleDesignerShellySchedules(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	cl, err := s.shellyClientForID(r.Context(), id)
	if err != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	res, err := cl.ScheduleList(r.Context())
	if err != nil {
		http.Error(w, "schedule read failed", http.StatusBadGateway)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"jobs": res.Jobs, "rev": res.Rev})
}

// handleDesignerShellyScheduleCreate adds an on-board schedule job. Only a
// switch.set call is accepted (the one action the editor programs), so this
// endpoint can never schedule an arbitrary device RPC. Returns the id the
// device assigned. Route: POST /a/designer/shelly/{id}/schedule.
func (s *Server) handleDesignerShellyScheduleCreate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var job shellyapi.ScheduleJob
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&job); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(job.Timespec) == "" || len(job.Calls) == 0 {
		http.Error(w, "timespec and calls required", http.StatusBadRequest)
		return
	}
	for _, c := range job.Calls {
		if strings.ToLower(c.Method) != "switch.set" {
			http.Error(w, "only switch.set schedule calls are allowed", http.StatusBadRequest)
			return
		}
	}
	cl, err := s.shellyClientForID(r.Context(), id)
	if err != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	newID, err := cl.ScheduleCreate(r.Context(), job)
	if err != nil {
		s.engineLog.Warn("shelly schedule create failed", "component", "shelly", "err", err)
		http.Error(w, "schedule create failed", http.StatusBadGateway)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"id": newID})
}

// handleDesignerShellyScheduleDelete removes one or more on-board schedule
// jobs by id (a grouped on/off schedule is two jobs).
// Route: POST /a/designer/shelly/{id}/schedule/delete. Body: {"ids":[...]}.
func (s *Server) handleDesignerShellyScheduleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		IDs []int `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || len(body.IDs) == 0 {
		http.Error(w, "ids required", http.StatusBadRequest)
		return
	}
	cl, err := s.shellyClientForID(r.Context(), id)
	if err != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	for _, jid := range body.IDs {
		if err := cl.ScheduleDelete(r.Context(), jid); err != nil {
			s.engineLog.Warn("shelly schedule delete failed", "component", "shelly", "job", jid, "err", err)
			http.Error(w, "schedule delete failed", http.StatusBadGateway)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
