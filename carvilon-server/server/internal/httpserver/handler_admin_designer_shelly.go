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

// --- Device-level surface (Saison 21 - Shelly completeness) ---------------
// Everything below serves the cockpit's and the editor's DEVICE view: the
// full config tree for display, whitelisted sys/ui/ble writes, firmware +
// reboot behind client-side confirms, the dynamic Script and Webhook
// components, and the auth change that keeps CARVILON's stored password
// in sync in the same action. The device's MQTT (and cloud) settings are
// deliberately NOT writable here - provisioning owns them, and editing
// them would sever the broker link.

// sysConfigKeys whitelists the dotted sys paths the UI may write
// (confirmed on the Pro 4PM Shelly.GetConfig tree).
var sysConfigKeys = map[string]bool{
	"device.name": true, "device.eco_mode": true, "device.discoverable": true,
	"location.tz": true, "location.lat": true, "location.lon": true,
	"sntp.server": true,
}

// uiConfigKeys / bleConfigKeys: the ui + ble component keys (confirmed).
var uiConfigKeys = map[string]bool{"idle_brightness": true}
var bleConfigKeys = map[string]bool{"enable": true, "rpc.enable": true}

// webhookKeys whitelists the Webhook.Create/Update fields the UI sends.
var webhookKeys = map[string]bool{
	"id": true, "cid": true, "enable": true, "event": true, "name": true, "urls": true,
}

// nestFromDotted turns a whitelisted {"a.b": v} map into {"a":{"b":v}} -
// the shape the nested *.SetConfig components expect.
func nestFromDotted(flat map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range flat {
		parts := strings.Split(k, ".")
		m := out
		for i := 0; i < len(parts)-1; i++ {
			next, ok := m[parts[i]].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[parts[i]] = next
			}
			m = next
		}
		m[parts[len(parts)-1]] = v
	}
	return out
}

// filterConfig keeps only whitelisted keys of a posted config body.
func filterConfig(in map[string]any, allowed map[string]bool) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		if allowed[k] {
			out[k] = v
		}
	}
	return out
}

// readConfigBody decodes a {"config":{...}} POST body.
func readConfigBody(r *http.Request) (map[string]any, bool) {
	var body struct {
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Config == nil {
		return nil, false
	}
	return body.Config, true
}

// handleDesignerShellyDevice returns everything the device view renders in
// one round trip: identity, the FULL config tree (raw - the UI shows what
// the device reports), and the live network status blocks. Each part is
// best-effort so one failing RPC does not blank the whole view.
// Route: GET /a/designer/shelly/{id}/device.
func (s *Server) handleDesignerShellyDevice(w http.ResponseWriter, r *http.Request) {
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
	out := map[string]any{}
	if info, ierr := cl.GetDeviceInfo(r.Context()); ierr == nil && info != nil {
		out["info"] = info.Raw
	}
	tree, terr := cl.DeviceTree(r.Context())
	if terr != nil {
		// no config tree -> the view cannot render; a friendly error
		designerJSON(w, http.StatusOK, map[string]any{"ok": false, "error": shellyFriendlyError(terr)})
		return
	}
	out["ok"] = true
	out["config"] = json.RawMessage(tree)
	if ws, werr := cl.WifiGetStatus(r.Context()); werr == nil {
		out["wifiStatus"] = json.RawMessage(ws)
	}
	if es, eerr := cl.EthGetStatus(r.Context()); eerr == nil {
		out["ethStatus"] = json.RawMessage(es)
	}
	designerJSON(w, http.StatusOK, out)
}

// shellyComponentConfig is the shared body of the sys/ui/ble config POSTs:
// whitelist, optionally nest, write, report restart_required.
func (s *Server) shellyComponentConfig(w http.ResponseWriter, r *http.Request, allowed map[string]bool, nested bool,
	call func(context.Context, *shellyapi.Client, map[string]any) (bool, error)) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	cfgIn, ok := readConfigBody(r)
	if !ok {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	cfg := filterConfig(cfgIn, allowed)
	if len(cfg) == 0 {
		http.Error(w, "no writable config keys", http.StatusBadRequest)
		return
	}
	if nested {
		cfg = nestFromDotted(cfg)
	}
	cl, err := s.shellyClientForID(r.Context(), id)
	if err != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	restart, cerr := call(r.Context(), cl, cfg)
	if cerr != nil {
		s.engineLog.Warn("shelly device config failed", "component", "shelly", "err", cerr)
		http.Error(w, "config write failed", http.StatusBadGateway)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"restartRequired": restart})
}

func (s *Server) handleDesignerShellySysConfig(w http.ResponseWriter, r *http.Request) {
	s.shellyComponentConfig(w, r, sysConfigKeys, true,
		func(ctx context.Context, cl *shellyapi.Client, cfg map[string]any) (bool, error) {
			return cl.SysSetConfig(ctx, cfg)
		})
}

func (s *Server) handleDesignerShellyUIConfig(w http.ResponseWriter, r *http.Request) {
	s.shellyComponentConfig(w, r, uiConfigKeys, false,
		func(ctx context.Context, cl *shellyapi.Client, cfg map[string]any) (bool, error) {
			return cl.UISetConfig(ctx, cfg)
		})
}

func (s *Server) handleDesignerShellyBLEConfig(w http.ResponseWriter, r *http.Request) {
	s.shellyComponentConfig(w, r, bleConfigKeys, true,
		func(ctx context.Context, cl *shellyapi.Client, cfg map[string]any) (bool, error) {
			return cl.BLESetConfig(ctx, cfg)
		})
}

// shellySimpleAction is the shared body of parameterless device actions.
func (s *Server) shellySimpleAction(w http.ResponseWriter, r *http.Request, act func(context.Context, *shellyapi.Client) error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	cl, cerr := s.shellyClientForID(r.Context(), id)
	if cerr != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	if aerr := act(r.Context(), cl); aerr != nil {
		s.engineLog.Warn("shelly device action failed", "component", "shelly", "err", aerr)
		http.Error(w, "action failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDesignerShellyReboot restarts the device. The UI puts an explicit
// confirm in front - this endpoint is the plain action.
// Route: POST /a/designer/shelly/{id}/reboot.
func (s *Server) handleDesignerShellyReboot(w http.ResponseWriter, r *http.Request) {
	s.shellySimpleAction(w, r, func(ctx context.Context, cl *shellyapi.Client) error { return cl.Reboot(ctx) })
}

// handleDesignerShellyFactoryReset wipes the device back to factory
// state. Destructive beyond recovery: the UI enforces an extensive
// consequence dialog plus type-to-confirm before this endpoint is ever
// called; the endpoint itself is the plain action. The CARVILON device
// record deliberately survives - the operator removes or re-adopts.
// Route: POST /a/designer/shelly/{id}/factory-reset.
func (s *Server) handleDesignerShellyFactoryReset(w http.ResponseWriter, r *http.Request) {
	s.shellySimpleAction(w, r, func(ctx context.Context, cl *shellyapi.Client) error { return cl.FactoryReset(ctx) })
}

// handleDesignerShellyFWCheck asks the device for available firmware.
// Route: GET /a/designer/shelly/{id}/fw-check.
func (s *Server) handleDesignerShellyFWCheck(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	cl, cerr := s.shellyClientForID(r.Context(), id)
	if cerr != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	raw, ferr := cl.CheckForUpdate(r.Context())
	if ferr != nil {
		http.Error(w, "check failed", http.StatusBadGateway)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"updates": json.RawMessage(raw)})
}

// handleDesignerShellyFWUpdate starts an OTA update ("stable"/"beta"),
// behind the UI's explicit confirm.
// Route: POST /a/designer/shelly/{id}/fw-update.
func (s *Server) handleDesignerShellyFWUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stage string `json:"stage"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.Stage != "stable" && body.Stage != "beta" {
		http.Error(w, "stage must be stable or beta", http.StatusBadRequest)
		return
	}
	s.shellySimpleAction(w, r, func(ctx context.Context, cl *shellyapi.Client) error { return cl.Update(ctx, body.Stage) })
}

// --- Scripts (mJS) ---------------------------------------------------------

// handleDesignerShellyScripts lists the on-device scripts.
// Route: GET /a/designer/shelly/{id}/scripts.
func (s *Server) handleDesignerShellyScripts(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	cl, cerr := s.shellyClientForID(r.Context(), id)
	if cerr != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	list, lerr := cl.ScriptList(r.Context())
	if lerr != nil {
		http.Error(w, "script list failed", http.StatusBadGateway)
		return
	}
	if list == nil {
		list = []shellyapi.ScriptInfo{}
	}
	designerJSON(w, http.StatusOK, map[string]any{"scripts": list})
}

// handleDesignerShellyScriptCreate creates a script (optionally with
// initial code). Route: POST /a/designer/shelly/{id}/script.
func (s *Server) handleDesignerShellyScriptCreate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Name string `json:"name"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	cl, cerr := s.shellyClientForID(r.Context(), id)
	if cerr != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	sid, serr := cl.ScriptCreate(r.Context(), strings.TrimSpace(body.Name))
	if serr != nil {
		http.Error(w, "script create failed", http.StatusBadGateway)
		return
	}
	codeSaved := true
	if body.Code != "" {
		if perr := cl.ScriptPutCode(r.Context(), sid, body.Code); perr != nil {
			s.engineLog.Warn("shelly script initial code failed", "component", "shelly", "err", perr)
			codeSaved = false
		}
	}
	designerJSON(w, http.StatusOK, map[string]any{"id": sid, "codeSaved": codeSaved})
}

// shellyScriptID parses {id} (store) + {sid} (script).
func shellyScriptID(r *http.Request) (devID int64, sid int, ok bool) {
	devID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || devID <= 0 {
		return 0, 0, false
	}
	sid, err2 := strconv.Atoi(r.PathValue("sid"))
	if err2 != nil || sid < 0 {
		return 0, 0, false
	}
	return devID, sid, true
}

// handleDesignerShellyScriptCode reads (GET) or replaces (POST) a
// script's source. Route: /a/designer/shelly/{id}/script/{sid}/code.
func (s *Server) handleDesignerShellyScriptCode(w http.ResponseWriter, r *http.Request) {
	devID, sid, ok := shellyScriptID(r)
	if !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	cl, cerr := s.shellyClientForID(r.Context(), devID)
	if cerr != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		code, gerr := cl.ScriptGetCode(r.Context(), sid)
		if gerr != nil {
			http.Error(w, "script read failed", http.StatusBadGateway)
			return
		}
		designerJSON(w, http.StatusOK, map[string]any{"code": code})
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if perr := cl.ScriptPutCode(r.Context(), sid, body.Code); perr != nil {
		http.Error(w, "script write failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDesignerShellyScriptAction runs enable/start/stop/delete on one
// script. Route: POST /a/designer/shelly/{id}/script/{sid}/{action}.
func (s *Server) handleDesignerShellyScriptAction(w http.ResponseWriter, r *http.Request) {
	devID, sid, ok := shellyScriptID(r)
	if !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	action := r.PathValue("action")
	var enable bool
	if action == "config" {
		var body struct {
			Enable bool `json:"enable"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		enable = body.Enable
	}
	cl, cerr := s.shellyClientForID(r.Context(), devID)
	if cerr != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	var aerr error
	switch action {
	case "config":
		aerr = cl.ScriptSetEnable(r.Context(), sid, enable)
	case "start":
		aerr = cl.ScriptStart(r.Context(), sid)
	case "stop":
		aerr = cl.ScriptStop(r.Context(), sid)
	case "delete":
		aerr = cl.ScriptDelete(r.Context(), sid)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if aerr != nil {
		s.engineLog.Warn("shelly script action failed", "component", "shelly", "action", action, "err", aerr)
		http.Error(w, "script action failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Webhooks ---------------------------------------------------------------

// handleDesignerShellyWebhooks lists hooks + the device's supported event
// catalog. Route: GET /a/designer/shelly/{id}/webhooks.
func (s *Server) handleDesignerShellyWebhooks(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	cl, cerr := s.shellyClientForID(r.Context(), id)
	if cerr != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	out := map[string]any{}
	hooks, herr := cl.WebhookList(r.Context())
	if herr != nil {
		http.Error(w, "webhook list failed", http.StatusBadGateway)
		return
	}
	out["hooks"] = json.RawMessage(hooks)
	if sup, serr := cl.WebhookListSupported(r.Context()); serr == nil {
		out["supported"] = json.RawMessage(sup)
	}
	designerJSON(w, http.StatusOK, out)
}

// handleDesignerShellyWebhookCreate / Update / Delete manage hooks with a
// whitelisted field set (cid, enable, event, name, urls).
func (s *Server) handleDesignerShellyWebhookCreate(w http.ResponseWriter, r *http.Request) {
	s.shellyWebhookWrite(w, r, false)
}

func (s *Server) handleDesignerShellyWebhookUpdate(w http.ResponseWriter, r *http.Request) {
	s.shellyWebhookWrite(w, r, true)
}

func (s *Server) shellyWebhookWrite(w http.ResponseWriter, r *http.Request, update bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	params := filterConfig(body, webhookKeys)
	if update {
		wid, werr := strconv.Atoi(r.PathValue("wid"))
		if werr != nil || wid < 0 {
			http.Error(w, "bad hook id", http.StatusBadRequest)
			return
		}
		params["id"] = wid
	} else {
		delete(params, "id")
		if params["event"] == nil {
			http.Error(w, "event required", http.StatusBadRequest)
			return
		}
	}
	cl, cerr := s.shellyClientForID(r.Context(), id)
	if cerr != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	if update {
		if uerr := cl.WebhookUpdate(r.Context(), params); uerr != nil {
			http.Error(w, "webhook update failed", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	wid, werr := cl.WebhookCreate(r.Context(), params)
	if werr != nil {
		http.Error(w, "webhook create failed", http.StatusBadGateway)
		return
	}
	designerJSON(w, http.StatusOK, map[string]any{"id": wid})
}

func (s *Server) handleDesignerShellyWebhookDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	wid, werr := strconv.Atoi(r.PathValue("wid"))
	if werr != nil || wid < 0 {
		http.Error(w, "bad hook id", http.StatusBadRequest)
		return
	}
	cl, cerr := s.shellyClientForID(r.Context(), id)
	if cerr != nil {
		http.Error(w, "device unavailable", http.StatusNotFound)
		return
	}
	if derr := cl.WebhookDelete(r.Context(), wid); derr != nil {
		http.Error(w, "webhook delete failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Auth -------------------------------------------------------------------

// handleDesignerShellyAuth rotates the installation's Shelly HTTP auth
// password: it applies Shelly.SetAuth to EVERY active device (using the
// still-valid OLD password clients - a one-device rotation would strand
// the rest of the fleet with no working re-key path), then persists the
// new password as CARVILON's stored (AES-GCM) installation secret and
// rebuilds the client fleet. The critical rule: CARVILON must never
// lock itself out of its own devices. Devices that fail to rotate are
// reported back by address so the operator knows which ones still hold
// the old password. A SetAuth whose response is lost (timeout after the
// device already applied it) is verified with a new-password probe
// before being counted as failed.
// Route: POST /a/designer/shelly/{id}/auth (fleet-wide by design; the
// {id} is the panel the action came from).
func (s *Server) handleDesignerShellyAuth(w http.ResponseWriter, r *http.Request) {
	if s.platformCfg == nil || s.shellystore == nil {
		http.Error(w, "configuration store unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if len(body.Password) < 8 {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	active, lerr := s.shellystore.ListActive(r.Context())
	if lerr != nil || len(active) == 0 {
		http.Error(w, "no active devices", http.StatusServiceUnavailable)
		return
	}
	oldPassword, _ := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyShellyPassword)

	applied := 0
	var failed []string
	for _, d := range active {
		norm, ok := normalizeShellyAddr(d.Address)
		if !ok || norm == "" {
			failed = append(failed, d.Address)
			continue
		}
		cl := shellyapi.New(shellyapi.Options{Address: norm, Password: oldPassword})
		info, ierr := cl.GetDeviceInfo(r.Context())
		if ierr != nil || info.IDLabel() == "" {
			failed = append(failed, d.Address)
			continue
		}
		if aerr := cl.SetAuth(r.Context(), info.IDLabel(), body.Password); aerr != nil {
			// The response may have been lost AFTER the device applied
			// the change - probe with the NEW password before giving up.
			probe := shellyapi.New(shellyapi.Options{Address: norm, Password: body.Password})
			if _, perr := probe.GetDeviceInfo(r.Context()); perr != nil {
				failed = append(failed, d.Address)
				continue
			}
		}
		applied++
	}
	if applied == 0 {
		// nothing rotated - keep the stored password as it is
		http.Error(w, "no device accepted the new password - stored password unchanged", http.StatusBadGateway)
		return
	}
	// At least one device now requires the new password, so the stored
	// secret must follow no matter what - a failure here is surfaced
	// loudly, because a mismatch locks CARVILON out of its own devices.
	if perr := s.platformCfg.SetSecret(r.Context(), platformconfig.KeyShellyPassword, body.Password); perr != nil {
		s.engineLog.Error("shelly auth rotated on devices but STORING the password failed - store it manually under Settings NOW", "err", perr)
		http.Error(w, "devices updated but storing the password failed - set it under Settings immediately", http.StatusInternalServerError)
		return
	}
	s.rebuildShellyClients(r.Context())
	if len(failed) > 0 {
		s.engineLog.Warn("shelly auth rotation incomplete", "component", "shelly", "applied", applied, "failed", len(failed))
	}
	designerJSON(w, http.StatusOK, map[string]any{"applied": applied, "failed": failed})
}
