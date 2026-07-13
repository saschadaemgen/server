package engine

import (
	"fmt"
	"strings"
)

// This file is the engine's I/O adapter seam: drivers provide inputs
// (Source) and outputs (Sink) over logical Channels, the graph binds to
// LOGICAL channel names, and a binding table maps those to a driver +
// physical address. Adding a real driver (GPIO, MQTT, ...) is a new file
// that registers under its namespace prefix plus binding rows - no engine
// change. The layer lives in package engine because a Source feeds the
// graph through the unexported externalSetter contract.

// Channel describes one physical I/O point a driver exposes: a stable
// Address within the driver, a human Label, and the value Kind it
// carries (bool|float|text). It is the unit a Source produces and a Sink
// consumes.
type Channel struct {
	Address string
	Label   string
	Kind    Kind
}

// Source is a driver that feeds external values into the engine. The
// engine subscribes to an address; the driver invokes the callback (from
// any goroutine) whenever that channel's value changes. The callback
// routes into the engine's async tick queue (EnqueueInput), so a Source
// never touches the eval path.
type Source interface {
	// Channels lists the input channels this driver exposes.
	Channels() []Channel
	// Subscribe registers cb for the given channel address. cb is invoked
	// with each new value and must be cheap and non-blocking - it only
	// stages into the tick queue. A driver SHOULD deliver the channel's
	// current level (via cb at subscribe time or on the first edge);
	// until it does, the bound source node holds its zero value. Errors
	// on an unknown address.
	Subscribe(addr string, cb func(Value)) error
}

// Sink is a driver that receives engine outputs. The engine calls Write
// from inside a tick (single-threaded, under the engine lock) whenever a
// bound output changes, so Write MUST be non-blocking and MUST NOT call
// back into the engine synchronously - that would deadlock the
// non-reentrant tick. A real sink hands the value off (buffer/channel);
// only the value's delivery is the engine's concern.
type Sink interface {
	// Channels lists the output channels this driver accepts.
	Channels() []Channel
	// Write delivers a value to the given channel address. Errors on an
	// unknown address.
	Write(addr string, v Value) error
}

// ChannelConfig is an opaque, per-line option bag carried from the graph
// to the driver at bind time. The engine does not interpret it - GPIO
// bias / active level / debounce / initial state mean nothing to the core
// - it only routes it to drivers that opt in via Configurable. A nil or
// empty config means "driver defaults", so a binding without options
// behaves exactly as before this seam existed.
type ChannelConfig map[string]string

// Configurable is an optional driver capability. BindGraph hands a
// Configurable driver each bound line's options BEFORE it wires the line,
// so the driver can apply them when it requests the physical line:
// ConfigureInput precedes Subscribe, ConfigureOutput precedes the first
// Write (and may pre-acquire the output at its initial state). A driver
// that takes no options simply does not implement this interface, and the
// config is ignored. The driver owns the interpretation of the map - the
// engine only carries it.
type Configurable interface {
	ConfigureInput(addr string, cfg ChannelConfig) error
	ConfigureOutput(addr string, cfg ChannelConfig) error
}

// Driver namespace prefixes. The prefix is the first colon-delimited
// segment of a physical address ("virtual:btn0", "gpio:gpiochip0:17",
// "nfc:i2c-1:uid"). gpio/sys/mqtt/telegram/nfc have live drivers today,
// virtual is the in-memory test driver; esp remains a reserved seam - a
// future driver registers under its prefix with no engine change.
const (
	PrefixVirtual  = "virtual"
	PrefixGPIO     = "gpio"
	PrefixSys      = "sys"
	PrefixMQTT     = "mqtt"
	PrefixTelegram = "telegram"
	PrefixESP      = "esp"
	PrefixNFC      = "nfc"
	PrefixProtect  = "protect"
	PrefixMidea    = "midea"
)

// DriverRegistry maps a namespace prefix to its Source/Sink driver. A
// driver may fill both roles (registered under the same prefix for each).
// It is built and wired once per deployment (and per test, for
// determinism) - not a process-wide global - so each owner controls its
// drivers.
type DriverRegistry struct {
	sources map[string]Source
	sinks   map[string]Sink
}

// NewDriverRegistry returns an empty driver registry.
func NewDriverRegistry() *DriverRegistry {
	return &DriverRegistry{sources: map[string]Source{}, sinks: map[string]Sink{}}
}

// RegisterSource binds a Source driver to a namespace prefix (e.g.
// "virtual"). Panics on an empty prefix or a duplicate - a wiring error
// caught at startup.
func (r *DriverRegistry) RegisterSource(prefix string, s Source) {
	if prefix == "" {
		panic("engine: RegisterSource with empty prefix")
	}
	if _, dup := r.sources[prefix]; dup {
		panic("engine: duplicate source driver for prefix " + prefix)
	}
	r.sources[prefix] = s
}

// RegisterSink binds a Sink driver to a namespace prefix.
func (r *DriverRegistry) RegisterSink(prefix string, s Sink) {
	if prefix == "" {
		panic("engine: RegisterSink with empty prefix")
	}
	if _, dup := r.sinks[prefix]; dup {
		panic("engine: duplicate sink driver for prefix " + prefix)
	}
	r.sinks[prefix] = s
}

// PhysicalAddr is a driver-qualified physical I/O point: a namespace
// prefix and a driver-local address. It is the value side of a binding;
// the graph never names it directly.
type PhysicalAddr struct {
	Prefix string
	Addr   string
}

// String renders the canonical "prefix:addr" form.
func (p PhysicalAddr) String() string { return p.Prefix + ":" + p.Addr }

// ParsePhysical splits a "prefix:addr" reference (e.g. "virtual:btn0",
// "gpio:17"). The address may itself contain colons - only the first
// delimits the prefix - so it never collides with a graph edge's
// node:port (an address is only ever a node param or a table value,
// never an edge endpoint). ok is false if either side is empty.
func ParsePhysical(s string) (PhysicalAddr, bool) {
	prefix, addr, found := strings.Cut(s, ":")
	if !found || prefix == "" || addr == "" {
		return PhysicalAddr{}, false
	}
	return PhysicalAddr{Prefix: prefix, Addr: addr}, true
}

// BindingTable maps a graph's LOGICAL channel name (the value of a
// source.channel / sink.channel node's "channel" param) to a PhysicalAddr
// (driver + address). The graph is hardware-blind: swapping hardware
// edits one row here, never the graph.
type BindingTable map[string]PhysicalAddr

// BindGraph wires a built engine's I/O nodes to drivers through the
// binding table. For each source.channel node it subscribes the resolved
// Source to the engine's async input queue; for each sink.channel node it
// points the node's output at the resolved Sink. Call it ONCE after Build
// (it is single-shot - no unbind/rebind): the graph builds and validates
// without bindings, and an unbound I/O node is simply inert (a source
// holds its zero value, a sink's writes go nowhere). It returns a clear
// error - never a panic, and never deferring a misconfiguration to an
// async callback - on a missing binding, a node/type mismatch, an
// unregistered prefix (e.g. a reserved gpio: with no driver yet), a
// channel the driver does not expose, a channel/port kind mismatch, or a
// driver that rejects the address.
//
// configs carries each logical channel's per-line options (same key as
// the table - the node's "channel" param). For a driver that implements
// Configurable, BindGraph applies the options before wiring the line; for
// any other driver, or a nil/empty configs, binding is exactly as before.
func BindGraph(eng *Engine, g Graph, table BindingTable, configs map[string]ChannelConfig, reg *DriverRegistry) error {
	for _, n := range g.Nodes {
		switch n.Type {
		case TypeSourceChannel, TypeSourceChannelFloat, TypeSourceChannelText:
			pa, err := resolveBinding(n, table)
			if err != nil {
				return err
			}
			// Validate the node at bind time (symmetric with the sink
			// path) so a misconfiguration fails loudly here, not later in
			// a driver-callback goroutine via EnqueueInput's panic.
			if _, ok := eng.nodes[n.ID].(*sourceChannel); !ok {
				return fmt.Errorf("engine: node %q is not a %s in the built engine", n.ID, TypeSourceChannel)
			}
			src := reg.sources[pa.Prefix]
			if src == nil {
				return fmt.Errorf("engine: no source driver registered for prefix %q (node %q)", pa.Prefix, n.ID)
			}
			if err := checkChannelKind(src.Channels(), pa, n, false); err != nil {
				return err
			}
			// Options precede Subscribe so the driver requests the line with
			// the right bias/active level/debounce (defaults if unset).
			if c, ok := src.(Configurable); ok {
				if err := c.ConfigureInput(pa.Addr, configs[logicalName(n)]); err != nil {
					return fmt.Errorf("engine: configure input %s for node %q: %w", pa, n.ID, err)
				}
			}
			id := n.ID
			if err := src.Subscribe(pa.Addr, func(v Value) { eng.EnqueueInput(id, "out", v) }); err != nil {
				return fmt.Errorf("engine: subscribe %s for node %q: %w", pa, n.ID, err)
			}
		case TypeSinkChannel, TypeSinkChannelFloat, TypeSinkChannelText:
			pa, err := resolveBinding(n, table)
			if err != nil {
				return err
			}
			node, ok := eng.nodes[n.ID].(*sinkChannel)
			if !ok {
				return fmt.Errorf("engine: node %q is not a %s in the built engine", n.ID, TypeSinkChannel)
			}
			snk := reg.sinks[pa.Prefix]
			if snk == nil {
				return fmt.Errorf("engine: no sink driver registered for prefix %q (node %q)", pa.Prefix, n.ID)
			}
			if err := checkChannelKind(snk.Channels(), pa, n, true); err != nil {
				return err
			}
			// Options precede the first Write; a Configurable sink may
			// pre-acquire the output at its initial state here.
			if c, ok := snk.(Configurable); ok {
				if err := c.ConfigureOutput(pa.Addr, configs[logicalName(n)]); err != nil {
					return fmt.Errorf("engine: configure output %s for node %q: %w", pa, n.ID, err)
				}
			}
			addr := pa.Addr
			node.onWrite = func(v Value) { _ = snk.Write(addr, v) }
		}
	}
	return nil
}

// logicalName is the node's "channel" param - the key into both the
// binding table and the configs map.
func logicalName(n GraphNode) string {
	s, _ := n.Params["channel"].(string)
	return s
}

// checkChannelKind verifies the driver actually exposes pa.Addr and that
// the channel's Kind matches the bound node port's Kind, so a mistyped
// binding fails loudly at bind time instead of silently coercing at
// runtime. isInput selects the node's input ("in") over its output
// ("out").
func checkChannelKind(chans []Channel, pa PhysicalAddr, n GraphNode, isInput bool) error {
	d, ok := Lookup(n.Type)
	if !ok {
		return fmt.Errorf("engine: unknown type %q for node %q", n.Type, n.ID)
	}
	var want Kind
	if isInput {
		want, _ = portKind(d.Inputs, "in")
	} else {
		want, _ = portKind(d.Outputs, "out")
	}
	ck, ok := channelKind(chans, pa.Addr)
	if !ok {
		return fmt.Errorf("engine: driver for prefix %q exposes no channel %q (node %q)", pa.Prefix, pa.Addr, n.ID)
	}
	if ck != want {
		return fmt.Errorf("engine: channel %s kind %s does not match node %q port kind %s", pa, kindName(ck), n.ID, kindName(want))
	}
	return nil
}

// channelKind returns the Kind of the channel with the given address.
func channelKind(chans []Channel, addr string) (Kind, bool) {
	for _, c := range chans {
		if c.Address == addr {
			return c.Kind, true
		}
	}
	return 0, false
}

// resolveBinding reads a node's "channel" param (its logical name) and
// resolves it through the table to a PhysicalAddr.
func resolveBinding(n GraphNode, table BindingTable) (PhysicalAddr, error) {
	logical, _ := n.Params["channel"].(string)
	if logical == "" {
		return PhysicalAddr{}, fmt.Errorf("engine: node %q has no channel param", n.ID)
	}
	pa, ok := table[logical]
	if !ok {
		return PhysicalAddr{}, fmt.Errorf("engine: no binding for logical channel %q (node %q)", logical, n.ID)
	}
	return pa, nil
}
