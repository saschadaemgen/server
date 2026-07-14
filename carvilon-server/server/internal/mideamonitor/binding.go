package mideamonitor

// Logic Editor integration: the adopted Midea device appears as a generic,
// capability-driven DEVICE module. Its sensor readouts (device return-air +
// outdoor) are engine SOURCE channels; its standard-profile controls
// (setpoint/mode/fan) are engine SINK channels. Same monitor, same live
// connection as the Device Center cockpit - the editor and cockpit share one
// client, so a control wired in the editor drives the real device exactly like
// the cockpit does.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"carvilon.local/server/internal/engine"
)

// Fixed standard-profile capability tokens (the channel suffix after the id).
const (
	chDeviceTemp = "device_temp"
	chOutdoor    = "outdoor_temp"
	chSetpoint   = "setpoint"
	chMode       = "mode"
	chFan        = "fan_mode"
)

// Readout / Control describe the fixed capability ports of a Midea device for
// the designer catalog bridge. Channel carries the engine.PrefixMidea namespace.
type CapReadout struct {
	Token   string
	Label   string
	Unit    string
	Kind    string // "float"
	Channel string // "midea:<id>:<token>"
}

// Control is one control capability as an editor INPUT port. Kind "text" enums
// carry Options (mode/fan_mode); setpoint is "float".
type CapControl struct {
	Token   string
	Label   string
	Unit    string
	Kind    string
	Options []string
	Channel string
}

// DeviceCaps is one adopted device as a capability-driven device module.
type DeviceCaps struct {
	ID       string
	Name     string
	Model    string
	Online   bool
	Readouts []CapReadout
	Controls []CapControl
}

// chanRef is the FULL, catalog-facing channel ref the editor bakes into the
// dropped node ("midea:<id>:<token>"). engAddr is the prefix-STRIPPED form the
// engine hands the driver after ParsePhysical splits off the namespace
// ("<id>:<token>") - engine Channels, Subscribe and Write all key on this.
func chanRef(id, token string) string { return engine.PrefixMidea + ":" + id + ":" + token }
func engAddr(id, token string) string { return id + ":" + token }

// parseChan splits the ENGINE-facing "<id>:<token>" (prefix already stripped)
// into id + token. The device id is lowercase hex, so it never contains a colon.
func parseChan(addr string) (id, token string, ok bool) {
	i := strings.LastIndex(addr, ":")
	if i <= 0 || i == len(addr)-1 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}

func mideaReadouts(id string) []CapReadout {
	return []CapReadout{
		{Token: chDeviceTemp, Label: "Return air", Unit: "°C", Kind: "float", Channel: chanRef(id, chDeviceTemp)},
		{Token: chOutdoor, Label: "Outdoor", Unit: "°C", Kind: "float", Channel: chanRef(id, chOutdoor)},
	}
}

func mideaControls(id string) []CapControl {
	return []CapControl{
		{Token: chSetpoint, Label: "Set temperature", Unit: "°C", Kind: "float", Channel: chanRef(id, chSetpoint)},
		{Token: chMode, Label: "Mode", Kind: "text", Options: []string{"off", "cool", "heat", "dry", "fan_only", "auto"}, Channel: chanRef(id, chMode)},
		{Token: chFan, Label: "Fan", Kind: "text", Options: []string{"auto", "low", "mid", "high"}, Channel: chanRef(id, chFan)},
	}
}

// Devices returns every adopted device as a capability-driven device module for
// the catalog. The standard-profile capability set is FIXED (not runtime
// present-filtered), so a device is wireable even before its first poll.
func (m *Monitor) Devices() []DeviceCaps {
	if m == nil {
		return nil
	}
	snap := m.Snapshot()
	out := make([]DeviceCaps, 0, len(snap))
	for _, r := range snap {
		out = append(out, DeviceCaps{
			ID:       r.ID,
			Online:   r.Online,
			Model:    "Midea Split AC",
			Readouts: mideaReadouts(r.ID),
			Controls: mideaControls(r.ID),
		})
	}
	return out
}

func kindOf(s string) engine.Kind {
	switch s {
	case "float":
		return engine.Float
	case "text":
		return engine.Text
	default:
		return engine.Bool
	}
}

// RunBinding is a per-run engine driver for the Midea devices: a SOURCE over the
// sensor readouts and a SINK over the controls. Register the SAME instance under
// engine.PrefixMidea for both roles and Close it on teardown.
type RunBinding struct {
	m            *Monitor
	readoutChans []engine.Channel
	controlChans []engine.Channel
	allChans     []engine.Channel

	mu     sync.Mutex
	subs   map[string]func(engine.Value)
	last   map[string]engine.Value
	closed bool

	cmds chan ctrlCmd
	done chan struct{}
}

type ctrlCmd struct {
	id, field, text string
	f               float64
}

// NewRunBinding builds a driver over the current adopted set.
func (m *Monitor) NewRunBinding() *RunBinding {
	b := &RunBinding{
		m:    m,
		subs: map[string]func(engine.Value){},
		last: map[string]engine.Value{},
		cmds: make(chan ctrlCmd, 64),
		done: make(chan struct{}),
	}
	for _, d := range m.Devices() {
		label := d.Name
		if label == "" {
			label = "Midea " + d.ID
		}
		for _, ro := range d.Readouts {
			b.readoutChans = append(b.readoutChans, engine.Channel{Address: engAddr(d.ID, ro.Token), Label: label + " " + ro.Label, Kind: engine.Float})
		}
		for _, c := range d.Controls {
			b.controlChans = append(b.controlChans, engine.Channel{Address: engAddr(d.ID, c.Token), Label: label + " " + c.Label, Kind: kindOf(c.Kind)})
		}
	}
	b.allChans = append(append([]engine.Channel(nil), b.readoutChans...), b.controlChans...)

	m.mu.Lock()
	m.bindings[b] = struct{}{}
	m.mu.Unlock()
	go b.worker()
	return b
}

// Channels lists both the readout (source) and control (sink) channels - the
// engine routes source.channel* addresses to Subscribe and sink.channel* to
// Write, so a driver filling both roles exposes the union here.
func (b *RunBinding) Channels() []engine.Channel {
	return append([]engine.Channel(nil), b.allChans...)
}

// Subscribe binds cb to a readout channel and delivers the current value.
func (b *RunBinding) Subscribe(addr string, cb func(engine.Value)) error {
	if !addrIn(b.readoutChans, addr) {
		return fmt.Errorf("mideamonitor: unknown readout channel %q", addr)
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return fmt.Errorf("mideamonitor: binding closed")
	}
	if _, dup := b.subs[addr]; dup {
		b.mu.Unlock()
		return fmt.Errorf("mideamonitor: channel %q already bound", addr)
	}
	b.subs[addr] = cb
	b.mu.Unlock()
	if v, ok := b.m.currentReadout(addr); ok {
		b.deliver(addr, v)
	}
	return nil
}

// Write delivers a control value. It is called from inside the engine tick, so
// it MUST NOT block: it enqueues the command for the worker goroutine (which
// does the device I/O off the eval path).
func (b *RunBinding) Write(addr string, v engine.Value) error {
	id, token, ok := parseChan(addr)
	if !ok {
		return fmt.Errorf("mideamonitor: unknown channel %q", addr)
	}
	// Single-driver exclusivity: while a control_loop run drives this device, the
	// device block's manual control sink is inert (the loop owns the device).
	if b.m.IsAutomatic(id) {
		return nil
	}
	cmd := ctrlCmd{id: id}
	switch token {
	case chSetpoint:
		cmd.field, cmd.f = "temp", v.F
	case chMode:
		cmd.field, cmd.text = "mode", v.S
	case chFan:
		cmd.field, cmd.text = "fan", v.S
	default:
		return fmt.Errorf("mideamonitor: not a control channel %q", addr)
	}
	select {
	case b.cmds <- cmd:
	default:
		b.m.log.Warn("midea run sink: command queue full, dropping", "addr", addr)
	}
	return nil
}

// Close stops the worker and detaches the binding.
func (b *RunBinding) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()
	close(b.done)
	b.m.mu.Lock()
	delete(b.m.bindings, b)
	b.m.mu.Unlock()
	return nil
}

// worker drains queued control commands and runs them off the tick path (each
// bounded; SetTemperature/SetMode/SetFan reconnect+retry internally).
func (b *RunBinding) worker() {
	for {
		select {
		case <-b.done:
			return
		case cmd := <-b.cmds:
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			var err error
			switch cmd.field {
			case "temp":
				err = b.m.SetTemperature(ctx, cmd.id, cmd.f)
			case "mode":
				err = b.m.SetMode(ctx, cmd.id, cmd.text)
			case "fan":
				err = b.m.SetFan(ctx, cmd.id, cmd.text)
			}
			cancel()
			if err != nil {
				b.m.log.Warn("midea run sink: control failed", "id", cmd.id, "field", cmd.field, "err", err)
			}
		}
	}
}

func (b *RunBinding) deliver(addr string, v engine.Value) {
	b.mu.Lock()
	cb := b.subs[addr]
	if cb == nil {
		// No subscriber yet: do NOT populate b.last, or a poll that lands before
		// Subscribe would make Subscribe's initial delivery dedup itself away and
		// the source node would sit at zero until a *different* reading arrives.
		b.mu.Unlock()
		return
	}
	prev, had := b.last[addr]
	b.last[addr] = v
	b.mu.Unlock()
	if !had || prev != v {
		cb(v)
	}
}

func addrIn(chans []engine.Channel, addr string) bool {
	for _, c := range chans {
		if c.Address == addr {
			return true
		}
	}
	return false
}

// currentReadout returns the cached value of a readout channel, if the device
// is tracked and has reported it.
func (m *Monitor) currentReadout(addr string) (engine.Value, bool) {
	id, token, ok := parseChan(addr)
	if !ok {
		return engine.Value{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ds := m.devs[id]
	if ds == nil || !ds.hasState {
		return engine.Value{}, false
	}
	switch token {
	case chDeviceTemp:
		if ds.last.HasTemp {
			return engine.FloatVal(ds.last.DeviceTempC), true
		}
	case chOutdoor:
		if ds.last.HasOutdoor {
			return engine.FloatVal(ds.last.OutdoorC), true
		}
	}
	return engine.Value{}, false
}

// pushReadouts delivers a device's current readout values to every run binding
// (called after a successful poll). Non-blocking: the binding callbacks only
// stage into the engine's tick queue.
func (m *Monitor) pushReadouts(id string) {
	m.mu.Lock()
	ds := m.devs[id]
	if ds == nil || !ds.hasState || len(m.bindings) == 0 {
		m.mu.Unlock()
		return
	}
	vals := map[string]engine.Value{}
	if ds.last.HasTemp {
		vals[engAddr(id, chDeviceTemp)] = engine.FloatVal(ds.last.DeviceTempC)
	}
	if ds.last.HasOutdoor {
		vals[engAddr(id, chOutdoor)] = engine.FloatVal(ds.last.OutdoorC)
	}
	bs := make([]*RunBinding, 0, len(m.bindings))
	for b := range m.bindings {
		bs = append(bs, b)
	}
	m.mu.Unlock()

	for _, b := range bs {
		for addr, v := range vals {
			b.deliver(addr, v)
		}
	}
}
