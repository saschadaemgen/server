package engine

import "fmt"

// VirtualDriver is an in-memory Source+Sink for deterministic tests and
// local experiments: no wall clock, no network, no goroutines of its
// own. It is registered per owner (not via init) under the "virtual"
// prefix for either role. A pre-declared set of channel addresses bounds
// Subscribe/Write; SetSource pushes an input edge into the engine via the
// bound callback, and every Write is recorded for assertion via
// SinkWrites.
//
// Concurrency: callbacks (cbs) are registered once at bind time, before
// any driving, then only read - safe to invoke SetSource from another
// goroutine (the value lands in the engine's e.mu-guarded queue). Sink
// writes (got) are appended only from inside a tick, on the goroutine
// that calls Tick, and read between ticks on that same goroutine.
type VirtualDriver struct {
	chans map[string]Channel
	cbs   map[string]func(Value) // source subscriptions, by address
	got   map[string][]Value     // sink writes, by address
}

// NewVirtualDriver declares the channel addresses the driver exposes.
// Subscribe and Write reject any address not declared here.
func NewVirtualDriver(chans ...Channel) *VirtualDriver {
	d := &VirtualDriver{
		chans: make(map[string]Channel, len(chans)),
		cbs:   map[string]func(Value){},
		got:   map[string][]Value{},
	}
	for _, c := range chans {
		d.chans[c.Address] = c
	}
	return d
}

// Channels lists the declared channels (Source and Sink share them).
func (d *VirtualDriver) Channels() []Channel {
	out := make([]Channel, 0, len(d.chans))
	for _, c := range d.chans {
		out = append(out, c)
	}
	return out
}

// Subscribe registers the engine callback for a source address.
func (d *VirtualDriver) Subscribe(addr string, cb func(Value)) error {
	if _, ok := d.chans[addr]; !ok {
		return fmt.Errorf("virtual: unknown channel %q", addr)
	}
	d.cbs[addr] = cb
	return nil
}

// Write records an engine output for a sink address. It is called from
// inside a tick (under the engine lock); it only appends - never
// re-entering the engine - so it cannot deadlock the non-reentrant tick.
func (d *VirtualDriver) Write(addr string, v Value) error {
	if _, ok := d.chans[addr]; !ok {
		return fmt.Errorf("virtual: unknown channel %q", addr)
	}
	d.got[addr] = append(d.got[addr], v)
	return nil
}

// SetSource simulates an external input edge: it invokes the engine
// callback bound to addr, which stages the value into the tick queue
// (EnqueueInput). Safe to call from any goroutine - the value lands at
// the next Tick. A no-op if addr was never subscribed.
func (d *VirtualDriver) SetSource(addr string, v Value) {
	if cb := d.cbs[addr]; cb != nil {
		cb(v)
	}
}

// SinkWrites returns the values written to a sink address, in order.
// Read it between ticks: the engine appends during a tick on the same
// goroutine that calls Tick.
func (d *VirtualDriver) SinkWrites(addr string) []Value {
	return d.got[addr]
}
