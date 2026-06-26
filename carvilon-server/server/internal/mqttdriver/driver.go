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
package mqttdriver

import (
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
// addresses the graph actually bound - the address IS the topic.
type Driver struct {
	client mqttbroker.InlineClient
	log    *slog.Logger

	chans []engine.Channel
	kinds map[string]engine.Kind // topic -> value kind

	mu     sync.Mutex
	retain map[string]bool // topic -> retain flag (sink option)
	subID  map[string]int  // subscribed topic -> inline subscription id
	nextID int
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

type pubMsg struct {
	topic   string
	payload []byte
	retain  bool
}

// NewDriver builds a driver for the given channels (each a topic +
// Kind derived from its graph node). client is the broker's inline
// pub/sub surface; it must be non-nil (the caller checks the broker is
// running before constructing the driver).
func NewDriver(client mqttbroker.InlineClient, channels []engine.Channel, log *slog.Logger) *Driver {
	if log == nil {
		log = slog.Default()
	}
	d := &Driver{
		client: client,
		log:    log.With("component", "mqtt-driver"),
		chans:  append([]engine.Channel(nil), channels...),
		kinds:  make(map[string]engine.Kind, len(channels)),
		retain: map[string]bool{},
		subID:  map[string]int{},
	}
	for _, c := range channels {
		d.kinds[c.Address] = c.Kind
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

// ConfigureInput is a no-op: an MQTT source has no per-line options in
// step 1. It exists so the driver satisfies Configurable uniformly
// with the sink path.
func (d *Driver) ConfigureInput(addr string, cfg engine.ChannelConfig) error { return nil }

// ConfigureOutput records the sink's retain option before the first
// Write. An absent or non-"true" value means retain off (QoS-0,
// fire-and-forget), the default.
func (d *Driver) ConfigureOutput(addr string, cfg engine.ChannelConfig) error {
	if _, ok := d.kinds[addr]; !ok {
		return fmt.Errorf("mqttdriver: unknown channel %q", addr)
	}
	retain := false
	switch strings.ToLower(strings.TrimSpace(cfg["retain"])) {
	case "true", "1", "on", "yes":
		retain = true
	}
	d.mu.Lock()
	d.retain[addr] = retain
	d.mu.Unlock()
	return nil
}

// Subscribe wires an engine callback to a topic. Each inbound message
// is parsed per the channel's Kind and handed to cb (which stages it
// into the engine's tick queue). A payload that does not parse for the
// kind is dropped with a debug log - a bad message must not crash the
// run or enqueue a garbage value.
func (d *Driver) Subscribe(addr string, cb func(engine.Value)) error {
	kind, ok := d.kinds[addr]
	if !ok {
		return fmt.Errorf("mqttdriver: unknown channel %q", addr)
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

	handler := func(topic string, payload []byte) {
		v, ok := parsePayload(kind, payload)
		if !ok {
			d.log.Debug("dropping unparseable mqtt payload", "topic", topic, "kind", kindName(kind))
			return
		}
		cb(v)
	}
	if err := d.client.Subscribe(addr, id, handler); err != nil {
		d.mu.Lock()
		delete(d.subID, addr)
		d.mu.Unlock()
		return fmt.Errorf("mqttdriver: subscribe %q: %w", addr, err)
	}
	return nil
}

// Write stages an engine output value for publishing to a topic,
// formatted per the channel's Kind, honouring the sink's retain option
// at QoS 0. Called from inside a tick (single-threaded, under the
// engine lock), so it only enqueues and returns - the worker performs
// the actual inline publish off the tick goroutine. On a full queue it
// drops with a debug log rather than block the tick.
func (d *Driver) Write(addr string, v engine.Value) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.kinds[addr]; !ok {
		return fmt.Errorf("mqttdriver: unknown channel %q", addr)
	}
	if d.closed {
		return nil // run tearing down; drop
	}
	msg := pubMsg{topic: addr, payload: formatPayload(v), retain: d.retain[addr]}
	select {
	case d.pub <- msg:
	default:
		d.log.Debug("mqtt publish queue full; dropping", "topic", addr)
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
		if err := d.client.Unsubscribe(addr, id); err != nil {
			d.log.Debug("mqtt unsubscribe failed", "topic", addr, "err", err)
		}
	}
	return nil
}

// parsePayload converts a raw MQTT payload to an engine Value per kind.
// ok is false for an unparseable payload (the caller drops it).
func parsePayload(kind engine.Kind, payload []byte) (engine.Value, bool) {
	s := strings.TrimSpace(string(payload))
	switch kind {
	case engine.Bool:
		switch strings.ToLower(s) {
		case "true", "1", "on":
			return engine.BoolVal(true), true
		case "false", "0", "off":
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
// Configurable (sink retain), and a Closer.
var (
	_ engine.Source       = (*Driver)(nil)
	_ engine.Sink         = (*Driver)(nil)
	_ engine.Configurable = (*Driver)(nil)
	_ io.Closer           = (*Driver)(nil)
)
