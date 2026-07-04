package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/gpio"
	"carvilon.local/server/internal/mqttbroker"
	"carvilon.local/server/internal/mqttdriver"
	"carvilon.local/server/internal/nfc"
	"carvilon.local/server/internal/sysmetrics"
	"carvilon.local/server/internal/telegrambot"
	"carvilon.local/server/internal/telegramdriver"
)

// designerRunTick is the wall-clock period the editor's live run advances
// the logical clock by, matching engine-monitor-demo (100ms ticks).
const designerRunTick = 100 * time.Millisecond

// designerRun is one live engine run bound to an admin session: a built
// engine driven by a wall-clock ticker. done stops both the ticker and
// the monitor SSE; closing it is idempotent.
type designerRun struct {
	eng     *engine.Engine
	done    chan struct{}
	once    sync.Once
	cleanup func() // release bound driver I/O (e.g. GPIO lines) on teardown
}

func (r *designerRun) stop() { r.once.Do(func() { close(r.done) }) }

// loop drives the engine on the wall clock until the run is stopped. The
// editor injects input out-of-band (SetInput) between ticks; each tick
// then settles the graph and fans a Frame out to the monitor SSE. On
// exit it releases any bound driver I/O: cleanup runs once, in this
// goroutine, after the final tick, so it never overlaps a Tick (a driver
// Write during eval and the cleanup's Close can't race).
func (r *designerRun) loop(tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	if r.cleanup != nil {
		defer r.cleanup()
	}
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			r.eng.Tick()
		}
	}
}

// setInput injects a value, recovering from the engine's panic on an
// unknown or non-source node so a malformed request can't crash the
// server. Returns false when the node does not accept external input.
func (r *designerRun) setInput(node, port string, v engine.Value) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	r.eng.SetInput(node, port, v)
	return true
}

// designerRunSet holds at most one live run per admin user. Starting a
// new run replaces (and stops) the user's previous one.
type designerRunSet struct {
	mu     sync.Mutex
	byUser map[string]*designerRun
}

func newDesignerRunSet() *designerRunSet {
	return &designerRunSet{byUser: map[string]*designerRun{}}
}

func (s *designerRunSet) start(user string, eng *engine.Engine, tick time.Duration, cleanup func()) *designerRun {
	run := &designerRun{eng: eng, done: make(chan struct{}), cleanup: cleanup}
	s.mu.Lock()
	old := s.byUser[user]
	s.byUser[user] = run
	s.mu.Unlock()
	if old != nil {
		old.stop()
	}
	go run.loop(tick)
	return run
}

func (s *designerRunSet) get(user string) *designerRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byUser[user]
}

// stopUser stops and forgets the user's current run (explicit Stop).
// Reports whether a run was actually running, so the caller can log
// the lifecycle without noise on idempotent re-stops.
func (s *designerRunSet) stopUser(user string) bool {
	s.mu.Lock()
	run := s.byUser[user]
	delete(s.byUser, user)
	s.mu.Unlock()
	if run != nil {
		run.stop()
	}
	return run != nil
}

// stopIfCurrent tears down a specific run on monitor disconnect, but only
// unmaps it while it is still the active one (so a reconnect that already
// started a newer run is left intact). The run is stopped regardless —
// stopping an already-replaced run is a harmless idempotent no-op.
// Reports whether it unmapped the active run, so exactly one lifecycle
// log line fires per run end (an explicit Stop usually unmaps first,
// making this return false).
func (s *designerRunSet) stopIfCurrent(user string, run *designerRun) bool {
	s.mu.Lock()
	current := s.byUser[user] == run
	if current {
		delete(s.byUser, user)
	}
	s.mu.Unlock()
	run.stop()
	return current
}

// handleDesignerRun validates+builds the posted canonical graph and, on
// success, starts a live run for the admin user. Validation errors come
// back as issues with HTTP 400 and nothing is executed.
func (s *Server) handleDesignerRun(w http.ResponseWriter, r *http.Request) {
	user := AdminUserFromContext(r.Context())
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "could not read request body"})
		return
	}
	g, err := engine.ParseGraph(body)
	if err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid graph JSON"})
		return
	}
	eng, err := engine.Build(g, engine.DefaultRegistry(), designerRunTick)
	if err != nil {
		var ve *engine.ValidationError
		if errors.As(err, &ve) {
			designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "issues": ve.Issues})
			return
		}
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cleanup, err := s.bindRunIO(eng, g)
	if err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.designerRuns.start(user, eng, designerRunTick, cleanup)
	// Engine lifecycle into the server log (and the System Log tab):
	// which admin started what size of graph.
	s.engineLog.Info("designer run started",
		"user", user, "nodes", len(g.Nodes), "edges", len(g.Edges))
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// isChannelNode reports whether a node type is one of the engine's I/O
// channel nodes (Bool/Float/Text source + sink) that the binding table
// resolves to a driver.
func isChannelNode(typ string) bool {
	switch typ {
	case engine.TypeSourceChannel, engine.TypeSourceChannelFloat, engine.TypeSourceChannelText,
		engine.TypeSinkChannel, engine.TypeSinkChannelFloat, engine.TypeSinkChannelText:
		return true
	}
	return false
}

// buildBindingTable derives the run's logical->physical binding table from
// its I/O channel nodes (any kind). Each such node's "channel" param is a
// physical reference "prefix:addr" (e.g. "gpio:gpiochip0:17",
// "sys:cpu_temp"), which the table maps through to its PhysicalAddr.
func buildBindingTable(g engine.Graph) (engine.BindingTable, error) {
	table := engine.BindingTable{}
	usedBy := map[string]string{} // physical channel -> the node that bound it
	for _, n := range g.Nodes {
		if !isChannelNode(n.Type) {
			continue
		}
		ref, _ := n.Params["channel"].(string)
		pa, ok := engine.ParsePhysical(ref)
		if !ok {
			return nil, fmt.Errorf("node %q: invalid channel %q (want prefix:addr, e.g. gpio:gpiochip0:17)", n.ID, ref)
		}
		// One physical channel maps to one node. Binding the same address to
		// two nodes would request a GPIO line twice, or fan one telemetry
		// metric to two callbacks where only the last survives - reject it
		// loudly rather than fail silently.
		if prev, dup := usedBy[pa.String()]; dup {
			return nil, fmt.Errorf("physical channel %s is bound by both node %q and node %q (one channel per node)", pa, prev, n.ID)
		}
		usedBy[pa.String()] = n.ID
		table[ref] = pa
	}
	return table, nil
}

// buildChannelConfigs collects each I/O node's per-line options (every
// param except "channel": bias / active_level / debounce_ms / initial)
// into a ChannelConfig keyed by the same logical ref as the binding
// table, so BindGraph hands them to the driver. A node with no options
// yields no entry and the driver applies its defaults (input: pull-up +
// active-low) - no regression for graphs from before this ticket.
func buildChannelConfigs(g engine.Graph) map[string]engine.ChannelConfig {
	configs := map[string]engine.ChannelConfig{}
	for _, n := range g.Nodes {
		if !isChannelNode(n.Type) {
			continue
		}
		ref, _ := n.Params["channel"].(string)
		if ref == "" {
			continue
		}
		cfg := engine.ChannelConfig{}
		for k, v := range n.Params {
			if k == "channel" {
				continue
			}
			if s, ok := v.(string); ok && s != "" {
				cfg[k] = s
			}
		}
		if len(cfg) > 0 {
			configs[ref] = cfg
		}
	}
	return configs
}

// buildMQTTChannels derives the mqtt: driver's channel set from the
// graph's MQTT I/O nodes: each such node's "channel" param is
// "mqtt:<topic>", and its node type fixes the value Kind. The topic is
// the driver-local address. Topics are free text (no host discovery),
// so the channel list IS whatever the graph binds.
func buildMQTTChannels(g engine.Graph) ([]engine.Channel, error) {
	var out []engine.Channel
	for _, n := range g.Nodes {
		if !isChannelNode(n.Type) {
			continue
		}
		ref, _ := n.Params["channel"].(string)
		pa, ok := engine.ParsePhysical(ref)
		if !ok || pa.Prefix != engine.PrefixMQTT {
			continue
		}
		kind, ok := channelKindForType(n.Type)
		if !ok {
			return nil, fmt.Errorf("node %q: cannot derive value kind from type %q", n.ID, n.Type)
		}
		out = append(out, engine.Channel{Address: pa.Addr, Label: pa.Addr, Kind: kind})
	}
	return out, nil
}

// channelKindForType maps an I/O channel node type to the value Kind it
// carries (bool variant has no suffix; float/text are suffixed).
func channelKindForType(typ string) (engine.Kind, bool) {
	switch typ {
	case engine.TypeSourceChannel, engine.TypeSinkChannel:
		return engine.Bool, true
	case engine.TypeSourceChannelFloat, engine.TypeSinkChannelFloat:
		return engine.Float, true
	case engine.TypeSourceChannelText, engine.TypeSinkChannelText:
		return engine.Text, true
	}
	return 0, false
}

// mqttInline returns the broker's in-process pub/sub client when the
// broker is wired and running, for the mqtt: driver to bind to.
func (s *Server) mqttInline() (mqttbroker.InlineClient, bool) {
	if s.mqtt == nil {
		return nil, false
	}
	return s.mqtt.Inline()
}

// buildTelegramChannels derives the telegram: driver's channel set from
// the graph's telegram I/O nodes: each such node's "channel" param is
// "telegram:<role>:<payload>[#slot]" and its node type fixes the value
// Kind. The address grammar (send:/cmd:/chat:, see telegramdriver) and
// the role/direction/kind fit are validated here so a bad block fails
// the run with a clear message instead of a driver surprise.
func buildTelegramChannels(g engine.Graph) ([]engine.Channel, error) {
	var out []engine.Channel
	for _, n := range g.Nodes {
		if !isChannelNode(n.Type) {
			continue
		}
		ref, _ := n.Params["channel"].(string)
		pa, ok := engine.ParsePhysical(ref)
		if !ok || pa.Prefix != engine.PrefixTelegram {
			continue
		}
		kind, ok := channelKindForType(n.Type)
		if !ok {
			return nil, fmt.Errorf("node %q: cannot derive value kind from type %q", n.ID, n.Type)
		}
		a, err := telegramdriver.ParseAddr(pa.Addr)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", n.ID, err)
		}
		isSource := strings.HasPrefix(n.Type, "source.")
		switch a.Role {
		case telegramdriver.RoleSend:
			if isSource {
				return nil, fmt.Errorf("node %q: telegram send:%d ist eine Senke, kein Eingang", n.ID, a.ChatID)
			}
			if kind != engine.Bool && kind != engine.Text {
				return nil, fmt.Errorf("node %q: telegram send erwartet Bool oder Text, nicht %q", n.ID, n.Type)
			}
		case telegramdriver.RoleCmd:
			if !isSource || kind != engine.Bool {
				return nil, fmt.Errorf("node %q: telegram cmd ist eine Bool-Quelle (Befehls-Puls)", n.ID)
			}
		case telegramdriver.RoleChat:
			if !isSource || kind != engine.Text {
				return nil, fmt.Errorf("node %q: telegram chat ist eine Text-Quelle (empfangener Text)", n.ID)
			}
		}
		out = append(out, engine.Channel{Address: pa.Addr, Label: pa.Addr, Kind: kind})
	}
	return out, nil
}

// validateTelegramChats enforces the allowlist at bind time: a send or
// chat channel naming a chat off the allowlist fails the run start with
// a pointer to /a/telegram (the runtime gate in the manager stays as
// defense in depth - the list can change mid-run). Command channels
// have no chat dimension: any allowlisted chat may trigger them.
func validateTelegramChats(chans []engine.Channel, allowed map[int64]string) error {
	for _, c := range chans {
		a, err := telegramdriver.ParseAddr(c.Address)
		if err != nil {
			return err // already validated; belt and braces
		}
		if a.Role == telegramdriver.RoleCmd {
			continue
		}
		if _, ok := allowed[a.ChatID]; !ok {
			return fmt.Errorf("telegram: chat %d ist nicht freigegeben (auf /a/telegram freigeben)", a.ChatID)
		}
	}
	return nil
}

// telegramConn returns the bot manager's in-process send/listen
// surface when the bot is wired and running, for the telegram: driver
// to bind to.
func (s *Server) telegramConn() (telegrambot.Conn, bool) {
	if s.telegram == nil || !s.telegram.Status().Running {
		return nil, false
	}
	return s.telegram, true
}

// bindRunIO wires a freshly built run's I/O nodes to their drivers. It
// registers a driver for each namespace the graph's channels actually use
// and the host exposes: gpio: (source+sink, requests the lines), sys:
// (source-only telemetry, starts a poller) and nfc: (source-only tag
// readers, claims the reader and starts a poller). It returns a cleanup
// that Close()s every registered driver on teardown (releasing GPIO
// lines, stopping the pollers). A graph with no I/O channels (the demo:
// input.manual/output.lamp) binds nothing and runs as before. A channel
// whose prefix has no driver here is rejected loudly by BindGraph.
func (s *Server) bindRunIO(eng *engine.Engine, g engine.Graph) (func(), error) {
	table, err := buildBindingTable(g)
	if err != nil {
		return nil, err
	}
	if len(table) == 0 {
		return func() {}, nil
	}
	configs := buildChannelConfigs(g)
	prefixes := map[string]bool{}
	for _, pa := range table {
		prefixes[pa.Prefix] = true
	}

	reg := engine.NewDriverRegistry()
	var closers []io.Closer
	cleanup := func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}

	if prefixes[engine.PrefixGPIO] && gpio.Enabled() {
		drv, err := gpio.NewDriver()
		if err != nil {
			return nil, fmt.Errorf("gpio driver: %w", err)
		}
		reg.RegisterSource(engine.PrefixGPIO, drv)
		reg.RegisterSink(engine.PrefixGPIO, drv)
		closers = append(closers, drv)
	}
	if prefixes[engine.PrefixSys] && sysmetrics.Enabled() {
		drv, err := sysmetrics.NewDriver()
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("sys driver: %w", err)
		}
		reg.RegisterSource(engine.PrefixSys, drv) // telemetry is read-only
		closers = append(closers, drv)
	}
	if prefixes[engine.PrefixNFC] && nfc.Enabled() {
		drv, err := nfc.NewDriver()
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("nfc driver: %w", err)
		}
		reg.RegisterSource(engine.PrefixNFC, drv) // tags are read-only
		closers = append(closers, drv)
	}
	if prefixes[engine.PrefixMQTT] {
		// MQTT topics ride on the broker's in-process inline client; a
		// graph that binds mqtt: channels needs the broker actually
		// running (the editor only offers the category when it is). The
		// channels (topic + kind) come from the graph nodes themselves -
		// topics are free text, so there is no fixed discovery list.
		client, ok := s.mqttInline()
		if !ok {
			cleanup()
			return nil, fmt.Errorf("mqtt driver: broker is not running (enable it on /a/mqtt)")
		}
		chans, err := buildMQTTChannels(g)
		if err != nil {
			cleanup()
			return nil, err
		}
		drv := mqttdriver.NewDriver(client, chans, s.log)
		reg.RegisterSource(engine.PrefixMQTT, drv)
		reg.RegisterSink(engine.PrefixMQTT, drv)
		closers = append(closers, drv)
	}
	if prefixes[engine.PrefixTelegram] {
		// Telegram chats ride on the bot manager's in-process Conn; a
		// graph that binds telegram: channels needs the bot actually
		// running (the editor only offers the category when it is). The
		// channels (chat/command + kind) come from the graph nodes
		// themselves; the allowlist is enforced at bind time AND per
		// message in the manager.
		conn, ok := s.telegramConn()
		if !ok {
			cleanup()
			return nil, fmt.Errorf("telegram driver: bot ist nicht aktiv (auf /a/telegram aktivieren)")
		}
		chans, err := buildTelegramChannels(g)
		if err != nil {
			cleanup()
			return nil, err
		}
		if err := validateTelegramChats(chans, s.telegram.AllowedChats()); err != nil {
			cleanup()
			return nil, err
		}
		drv, err := telegramdriver.NewDriver(conn, chans, s.log)
		if err != nil {
			cleanup()
			return nil, err
		}
		reg.RegisterSource(engine.PrefixTelegram, drv)
		reg.RegisterSink(engine.PrefixTelegram, drv)
		closers = append(closers, drv)
	}
	if err := engine.BindGraph(eng, g, table, configs, reg); err != nil {
		cleanup() // release any I/O opened before the failure
		return nil, err
	}
	return cleanup, nil
}

// handleDesignerRunMonitor streams the user's running engine as SSE: a
// "snapshot" event with the present value on every wire, then a "tick"
// event per changed tick (engine.Frame). It reuses the engine's
// Subscribe/Snapshot fan-out and tears the run down when the client
// disconnects (briefing: stop on disconnect).
func (s *Server) handleDesignerRunMonitor(w http.ResponseWriter, r *http.Request) {
	user := AdminUserFromContext(r.Context())
	run := s.designerRuns.get(user)
	if run == nil {
		http.Error(w, "no running graph", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	frames, cancel := run.eng.Subscribe(64)
	defer cancel()
	// Monitor disconnect ends the run (briefing rule). The editor's Stop
	// button closes the stream before POSTing run/stop, so THIS is the
	// usual place the run actually ends — log it here (and only when we
	// unmapped it, so an explicit stop that won the race logs instead).
	defer func() {
		if s.designerRuns.stopIfCurrent(user, run) {
			s.engineLog.Info("designer run stopped", "user", user)
		}
	}()

	if err := writeDesignerSSE(w, "snapshot", map[string]any{"changes": run.eng.Snapshot()}); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-run.done:
			return
		case f, ok := <-frames:
			if !ok {
				return
			}
			if err := writeDesignerSSE(w, "tick", f); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleDesignerRunInput injects the editor's button press into the
// running engine (SetInput on the input.manual node), driving real
// evaluation. A press is a true/false pulse the editor sends.
func (s *Server) handleDesignerRunInput(w http.ResponseWriter, r *http.Request) {
	user := AdminUserFromContext(r.Context())
	run := s.designerRuns.get(user)
	if run == nil {
		http.Error(w, "no running graph", http.StatusNotFound)
		return
	}
	var in struct {
		Node  string `json:"node"`
		Port  string `json:"port"`
		Value bool   `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad input body", http.StatusBadRequest)
		return
	}
	if in.Node == "" || in.Port == "" {
		http.Error(w, "node and port are required", http.StatusBadRequest)
		return
	}
	if !run.setInput(in.Node, in.Port, engine.BoolVal(in.Value)) {
		http.Error(w, "node does not accept input", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDesignerRunStop stops the user's run (idempotent).
func (s *Server) handleDesignerRunStop(w http.ResponseWriter, r *http.Request) {
	user := AdminUserFromContext(r.Context())
	if s.designerRuns.stopUser(user) {
		s.engineLog.Info("designer run stopped", "user", user)
	}
	w.WriteHeader(http.StatusNoContent)
}

func designerJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeDesignerSSE serializes one SSE event (event + data + blank line),
// mirroring enginehttp's writer (kept local so this package needs no
// access to the engine transport's unexported helper).
func writeDesignerSSE(w http.ResponseWriter, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
