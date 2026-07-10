// Package mqttdriver is the engine adapter-layer driver for the
// "mqtt:" namespace: it turns MQTT topics into engine Sources and
// Sinks, exactly like gpio: and sys: - a pure addition, the engine is
// unchanged. It rides on the embedded broker's in-process inline
// client (no TCP loopback, no device credentials: the engine talking
// to its own broker is first-party and bypasses the network auth/ACL
// by design).
//
// Determinism is preserved the same way GPIO edges and sys polls are:
// an inbound MQTT message never touches the eval path directly - the
// subscribe callback hands the parsed value to the engine's async
// EnqueueInput queue (wired by BindGraph), so it lands at the next
// tick, never concurrent with evaluation.
//
// # Address grammar
//
// A channel address is "<topic>[#<selector>]". The topic may contain
// colons and slashes (a Shelly channel is "carvilon/shelly-<mac>/status/
// switch:0") - only ParsePhysical's first colon splits the namespace, so
// the whole topic reaches here intact. '#' cannot appear in a published
// topic name (it is the MQTT multi-level wildcard), so it is a safe
// delimiter for the optional selector. The selector's meaning depends on
// the binding's DIRECTION - the node is a source XOR a sink, so there is
// no ambiguity:
//
//   - Source: the selector is a dot-notation JSON path into the payload
//     ("output", "apower", "temperature.tC", "aenergy.total"). Absent, the
//     whole (trimmed) payload is cast to the channel kind - so a flat
//     "online" topic ("true"/"false") still binds directly.
//   - Sink: the selector is an RPC target "<Method>:<id>" ("Switch.Set:0").
//     Absent, the formatted value is published to the topic verbatim (the
//     original behaviour). With it, a JSON-RPC envelope is published so a
//     Shelly relay switches over the documented Gen2 method - the device
//     then confirms the new state on its status/events topics, which a
//     source binding reads back.
//
// Putting the selector IN the address (not in ChannelConfig) keeps each
// physical channel self-describing and distinct: "status/switch:0#output"
// and "status/switch:0#apower", or "rpc#Switch.Set:0" and "rpc#Switch.Set:1",
// are separate physical channels on one topic, which the run layer's
// one-channel-per-node rule needs.
package mqttdriver

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/mqttbroker"
)

// Driver exposes a set of MQTT topics as engine channels over the
// broker's inline client. One instance per run; Close unsubscribes
// every source topic. The channel set is fixed at construction (built
// from the run's graph), so Channels/Subscribe/Write only ever see
// addresses the graph actually bound - the address (topic + optional
// selector) IS the channel identity.
type Driver struct {
	client mqttbroker.InlineClient
	log    *slog.Logger

	chans []engine.Channel
	info  map[string]*chanInfo // address -> parsed channel (immutable after New)

	mu     sync.Mutex
	retain map[string]bool    // sink address -> retain flag (plain-publish option)
	onOff  map[string]bool    // sink address -> bool payload as "on"/"off" (Gen1 grammar)
	rpc    map[string]rpcSpec // sink address -> RPC target (Switch.Set etc.)
	subID  map[string]int     // subscribed address -> inline subscription id
	nextID int
	rpcSeq int64 // monotonic JSON-RPC request id
	closed bool

	// pub decouples Write from the actual inline publish. Write runs
	// inside the engine tick (under the engine lock); the inline
	// publish delivers synchronously to inline subscribers, so a
	// publish that loops back to a same-topic source would re-enter
	// EnqueueInput and deadlock the non-reentrant tick. A single worker
	// drains pub and publishes off the tick goroutine, in order, so
	// Write only ever stages - exactly what the Sink contract requires.
	pub chan pubMsg
}

// chanInfo is a channel address parsed once at construction: its value
// kind, the MQTT topic (the subscribe filter / publish target), and the
// raw selector text after '#' ("" when none).
type chanInfo struct {
	kind     engine.Kind
	topic    string
	selector string
}

// rpcSpec is a sink's parsed RPC target: the Gen2 method and the
// component id it addresses (a relay index for Switch.Set).
type rpcSpec struct {
	method string
	id     int
}

type pubMsg struct {
	topic   string
	payload []byte
	retain  bool
}

// splitAddr separates a channel address into its topic and optional
// selector at the first '#'. '#' never occurs in a real topic name, so
// this is unambiguous.
func splitAddr(addr string) (topic, selector string) {
	if i := strings.IndexByte(addr, '#'); i >= 0 {
		return addr[:i], addr[i+1:]
	}
	return addr, ""
}

// NewDriver builds a driver for the given channels (each a topic +
// optional selector + Kind derived from its graph node). client is the
// broker's inline pub/sub surface; it must be non-nil (the caller checks
// the broker is running before constructing the driver).
func NewDriver(client mqttbroker.InlineClient, channels []engine.Channel, log *slog.Logger) *Driver {
	if log == nil {
		log = slog.Default()
	}
	d := &Driver{
		client: client,
		log:    log.With("component", "mqtt-driver"),
		chans:  append([]engine.Channel(nil), channels...),
		info:   make(map[string]*chanInfo, len(channels)),
		retain: map[string]bool{},
		onOff:  map[string]bool{},
		rpc:    map[string]rpcSpec{},
		subID:  map[string]int{},
	}
	for _, c := range channels {
		topic, selector := splitAddr(c.Address)
		d.info[c.Address] = &chanInfo{kind: c.Kind, topic: topic, selector: selector}
	}
	d.pub = make(chan pubMsg, 256)
	go d.publishLoop()
	return d
}

// publishLoop drains the outbound queue and publishes off the engine
// tick goroutine, preserving order. It exits when Close closes pub.
func (d *Driver) publishLoop() {
	for m := range d.pub {
		if err := d.client.Publish(m.topic, m.payload, m.retain, 0); err != nil {
			d.log.Debug("mqtt publish failed", "topic", m.topic, "err", err)
		}
	}
}

// Channels lists the topics this run binds, as engine channels.
func (d *Driver) Channels() []engine.Channel { return d.chans }

// ConfigureInput is a no-op: an MQTT source's only per-line detail (the
// JSON path) rides in the address selector, applied in Subscribe. It
// exists so the driver satisfies Configurable uniformly with the sink.
func (d *Driver) ConfigureInput(addr string, cfg engine.ChannelConfig) error { return nil }

// ConfigureOutput records the sink's options before the first Write: an
// RPC target parsed from the address selector (Shelly Gen2 relay
// switching), the plain-publish retain flag, and the plain-publish bool
// payload style - "" (default "true"/"false") or "on-off" ("on"/"off",
// the raw grammar Gen1 Shelly command topics expect; the Gen1 module's
// binding generator sets it). A malformed selector, an RPC target on a
// non-bool channel, or an unknown payload style fails the bind loudly
// rather than surfacing later inside a tick. Commands are never retained.
func (d *Driver) ConfigureOutput(addr string, cfg engine.ChannelConfig) error {
	ci, ok := d.info[addr]
	if !ok {
		return fmt.Errorf("mqttdriver: unknown channel %q", addr)
	}
	var spec *rpcSpec
	if ci.selector != "" {
		s, err := parseRPCSelector(ci.selector)
		if err != nil {
			return fmt.Errorf("mqttdriver: channel %q: %w", addr, err)
		}
		if ci.kind != engine.Bool {
			return fmt.Errorf("mqttdriver: %s sink requires a bool channel, got %s (node bound to %q)", s.method, kindName(ci.kind), addr)
		}
		spec = &s
	}
	retain := false
	switch strings.ToLower(strings.TrimSpace(cfg["retain"])) {
	case "true", "1", "on", "yes":
		retain = true
	}
	onOff := false
	switch style := strings.ToLower(strings.TrimSpace(cfg["payload"])); style {
	case "", "default":
	case "on-off":
		if ci.kind != engine.Bool {
			return fmt.Errorf("mqttdriver: payload style %q requires a bool channel, got %s (node bound to %q)", style, kindName(ci.kind), addr)
		}
		if spec != nil {
			return fmt.Errorf("mqttdriver: payload style %q does not apply to an rpc sink (node bound to %q)", style, addr)
		}
		onOff = true
	default:
		return fmt.Errorf("mqttdriver: unknown payload style %q (node bound to %q)", style, addr)
	}
	d.mu.Lock()
	if spec != nil {
		d.rpc[addr] = *spec
		retain = false // an RPC command is not a retained state
	}
	d.retain[addr] = retain
	d.onOff[addr] = onOff
	d.mu.Unlock()
	return nil
}

// parseRPCSelector parses a sink selector "<Method>:<id>" into an rpcSpec.
// It splits on the LAST colon so a dotted method name ("Switch.Set")
// stays intact. Phase 1 supports only Switch.Set; any other method is
// rejected with a clear error (the thin shelly: driver in phase 2 owns
// the wider surface).
func parseRPCSelector(sel string) (rpcSpec, error) {
	i := strings.LastIndexByte(sel, ':')
	if i <= 0 || i == len(sel)-1 {
		return rpcSpec{}, fmt.Errorf("invalid rpc selector %q (want Method:id, e.g. Switch.Set:0)", sel)
	}
	method, idStr := sel[:i], sel[i+1:]
	if method != "Switch.Set" {
		return rpcSpec{}, fmt.Errorf("unsupported rpc method %q (only Switch.Set in phase 1)", method)
	}
	id, err := strconv.Atoi(idStr)
	if err != nil || id < 0 {
		return rpcSpec{}, fmt.Errorf("invalid rpc component id %q in selector %q", idStr, sel)
	}
	return rpcSpec{method: method, id: id}, nil
}

// Subscribe wires an engine callback to a topic. Each inbound message is
// parsed per the channel's Kind - directly when the address has no
// selector, or by walking the selector's dot-notation JSON path into the
// payload first - and handed to cb (which stages it into the engine's
// tick queue). A payload that does not parse (bad JSON, missing path, or
// a leaf that will not cast) is dropped with a debug log: a bad message
// must not crash the run or enqueue a garbage value.
func (d *Driver) Subscribe(addr string, cb func(engine.Value)) error {
	ci, ok := d.info[addr]
	if !ok {
		return fmt.Errorf("mqttdriver: unknown channel %q", addr)
	}
	var path []string
	if ci.selector != "" {
		path = strings.Split(ci.selector, ".")
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return fmt.Errorf("mqttdriver: driver closed")
	}
	id := d.nextID
	d.nextID++
	d.subID[addr] = id
	d.mu.Unlock()

	kind := ci.kind
	sel := ci.selector
	handler := func(topic string, payload []byte) {
		var (
			v  engine.Value
			ok bool
		)
		if len(path) > 0 {
			leaf, found := extractPath(payload, path)
			if !found {
				d.log.Debug("mqtt json path not found", "topic", topic, "path", sel)
				return
			}
			v, ok = castValue(kind, leaf)
		} else {
			v, ok = parsePayload(kind, payload)
		}
		if !ok {
			d.log.Debug("dropping unparseable mqtt payload", "topic", topic, "kind", kindName(kind))
			return
		}
		cb(v)
	}
	if err := d.client.Subscribe(ci.topic, id, handler); err != nil {
		d.mu.Lock()
		delete(d.subID, addr)
		d.mu.Unlock()
		return fmt.Errorf("mqttdriver: subscribe %q: %w", ci.topic, err)
	}
	return nil
}

// Write stages an engine output value for publishing to a topic. A plain
// sink formats the value per its Kind and honours the retain option; an
// RPC sink builds a Gen2 JSON-RPC envelope (Switch.Set) instead, never
// retained. Called from inside a tick (single-threaded, under the engine
// lock), so it only enqueues and returns - the worker performs the actual
// inline publish off the tick goroutine. On a full queue it drops with a
// debug log rather than block the tick.
func (d *Driver) Write(addr string, v engine.Value) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	ci, ok := d.info[addr]
	if !ok {
		return fmt.Errorf("mqttdriver: unknown channel %q", addr)
	}
	if d.closed {
		return nil // run tearing down; drop
	}
	var (
		payload []byte
		retain  = d.retain[addr]
	)
	if spec, isRPC := d.rpc[addr]; isRPC {
		d.rpcSeq++
		payload = formatRPC(spec, ci.topic, d.rpcSeq, v.B)
		retain = false
	} else if d.onOff[addr] && v.Kind == engine.Bool {
		// the Gen1 command grammar: raw "on"/"off" instead of true/false
		if v.B {
			payload = []byte("on")
		} else {
			payload = []byte("off")
		}
	} else {
		payload = formatPayload(v)
	}
	select {
	case d.pub <- pubMsg{topic: ci.topic, payload: payload, retain: retain}:
	default:
		d.log.Debug("mqtt publish queue full; dropping", "topic", ci.topic)
	}
	return nil
}

// Close unsubscribes every source topic. Idempotent.
func (d *Driver) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	subs := make(map[string]int, len(d.subID))
	for addr, id := range d.subID {
		subs[addr] = id
	}
	d.subID = map[string]int{}
	close(d.pub) // stop the publish worker
	d.mu.Unlock()
	for addr, id := range subs {
		// info is immutable after construction - safe to read unlocked.
		if err := d.client.Unsubscribe(d.info[addr].topic, id); err != nil {
			d.log.Debug("mqtt unsubscribe failed", "topic", d.info[addr].topic, "err", err)
		}
	}
	return nil
}

// extractPath walks a dot-notation path into a JSON payload and returns
// the leaf value. found is false for a payload that is not a JSON object
// tree along the path (bad JSON, or a missing/non-object key), so the
// caller drops it.
func extractPath(payload []byte, path []string) (any, bool) {
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, false
	}
	cur := root
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// castValue coerces a JSON leaf (bool | number | string, as
// encoding/json decodes them) to an engine Value of the channel kind. It
// is tolerant across the natural crossings (a numeric Shelly field read
// as text, a 0/1 read as bool) but returns ok=false for a leaf that has
// no sensible representation in the kind (e.g. an object where a scalar
// was expected).
func castValue(kind engine.Kind, leaf any) (engine.Value, bool) {
	switch kind {
	case engine.Bool:
		switch x := leaf.(type) {
		case bool:
			return engine.BoolVal(x), true
		case float64:
			return engine.BoolVal(x != 0), true
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "true", "1", "on":
				return engine.BoolVal(true), true
			case "false", "0", "off":
				return engine.BoolVal(false), true
			}
		}
	case engine.Float:
		switch x := leaf.(type) {
		case float64:
			return engine.FloatVal(x), true
		case bool:
			if x {
				return engine.FloatVal(1), true
			}
			return engine.FloatVal(0), true
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				return engine.FloatVal(f), true
			}
		}
	case engine.Text:
		switch x := leaf.(type) {
		case string:
			return engine.TextVal(x), true
		case float64:
			return engine.TextVal(strconv.FormatFloat(x, 'g', -1, 64)), true
		case bool:
			if x {
				return engine.TextVal("true"), true
			}
			return engine.TextVal("false"), true
		}
	}
	return engine.Value{}, false
}

// formatRPC renders a Gen2 JSON-RPC request for an RPC sink. src is a
// response topic under the device's own prefix (the reply is ignored in
// phase 1 - the device confirms the new state on its status/events
// topics, which a source binding reads back). id is a monotonic request
// id. For Switch.Set, params carries the relay index and the on state.
func formatRPC(spec rpcSpec, topic string, id int64, on bool) []byte {
	req := map[string]any{
		"id":     id,
		"src":    topic + "/resp",
		"method": spec.method,
		"params": map[string]any{"id": spec.id, "on": on},
	}
	b, err := json.Marshal(req)
	if err != nil { // map[string]any of scalars never fails; belt and braces
		return nil
	}
	return b
}

// parsePayload converts a raw MQTT payload to an engine Value per kind
// for a selector-less (flat) channel. ok is false for an unparseable
// payload (the caller drops it).
func parsePayload(kind engine.Kind, payload []byte) (engine.Value, bool) {
	s := strings.TrimSpace(string(payload))
	switch kind {
	case engine.Bool:
		switch strings.ToLower(s) {
		case "true", "1", "on":
			return engine.BoolVal(true), true
		case "false", "0", "off", "overpower":
			// "overpower" is the Gen1 protective trip: the relay IS off -
			// dropping it would leave a bound source stuck at stale "on".
			return engine.BoolVal(false), true
		}
		return engine.Value{}, false
	case engine.Float:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return engine.Value{}, false
		}
		return engine.FloatVal(f), true
	case engine.Text:
		// Text takes the raw payload verbatim (untrimmed): whitespace
		// can be meaningful in a string value.
		return engine.TextVal(string(payload)), true
	default:
		return engine.Value{}, false
	}
}

// formatPayload renders an engine Value as an MQTT payload per its kind.
func formatPayload(v engine.Value) []byte {
	switch v.Kind {
	case engine.Bool:
		if v.B {
			return []byte("true")
		}
		return []byte("false")
	case engine.Float:
		return []byte(strconv.FormatFloat(v.F, 'g', -1, 64))
	case engine.Text:
		return []byte(v.S)
	default:
		return nil
	}
}

func kindName(k engine.Kind) string {
	switch k {
	case engine.Bool:
		return "bool"
	case engine.Float:
		return "float"
	case engine.Text:
		return "text"
	default:
		return "?"
	}
}

// Compile-time contract checks: the driver is a Source, a Sink, a
// Configurable (sink retain / rpc), and a Closer.
var (
	_ engine.Source       = (*Driver)(nil)
	_ engine.Sink         = (*Driver)(nil)
	_ engine.Configurable = (*Driver)(nil)
	_ io.Closer           = (*Driver)(nil)
)
