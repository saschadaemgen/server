package engine

import "sort"

// Change is one observed signal value on a wire, reported at the
// wire's destination (consuming) input port. This is the "live value
// on the line" the logic editor renders.
type Change struct {
	Node  string `json:"node"`
	Port  string `json:"port"`
	Value Value  `json:"value"`
}

// Frame is the set of signal changes produced by a single tick, plus
// the tick counter and the logical time it represents. The engine
// only emits a Frame for ticks that actually changed something.
type Frame struct {
	Tick    int64    `json:"tick"`    // tick counter
	TimeMs  int64    `json:"time_ms"` // logical milliseconds since start
	Changes []Change `json:"changes"`
}

// Subscribe registers a monitor observer and returns a buffered Frame
// channel plus a cancel function. The channel is buffered so a slow
// consumer never stalls the tick loop: when the buffer is full the
// frame is dropped for that subscriber (a freshly reconnecting client
// recovers via Snapshot). cancel removes the subscriber and closes
// the channel; it is safe to call exactly once and idempotent after.
func (e *Engine) Subscribe(buffer int) (<-chan Frame, func()) {
	if buffer < 0 {
		buffer = 0
	}
	ch := make(chan Frame, buffer)

	e.mu.Lock()
	e.subs[ch] = struct{}{}
	e.mu.Unlock()

	cancel := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if _, ok := e.subs[ch]; ok {
			delete(e.subs, ch)
			close(ch)
		}
	}
	return ch, cancel
}

// Snapshot returns the current value on every wire, keyed by the
// destination input port - the same coordinate system as Frame
// changes - so a newly connected observer sees the full present
// state before it starts receiving incremental frames. The result is
// sorted by node then port for a stable, reproducible ordering.
func (e *Engine) Snapshot() []Change {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]Change, 0, len(e.wires))
	// Each node's own output values, so a freshly connected observer sees a
	// source's present value even when it drives no wire (the card display).
	for node, ports := range e.outs {
		for port, v := range ports {
			out = append(out, Change{Node: node, Port: port, Value: v})
		}
	}
	// The destination (consuming) value on every wire - the live-on-the-line
	// coordinate the editor highlights.
	for src, dsts := range e.wires {
		v := e.outs[src.node][src.port]
		for _, dst := range dsts {
			out = append(out, Change{Node: dst.node, Port: dst.port, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Port < out[j].Port
	})
	return out
}

// fanout pushes a frame to every subscriber without blocking. It runs
// inside Tick with e.mu already held; a full subscriber buffer drops
// the frame rather than stalling the deterministic tick loop.
func (e *Engine) fanout(f Frame) {
	for ch := range e.subs {
		select {
		case ch <- f:
		default: // slow subscriber: drop this frame, it resyncs via Snapshot
		}
	}
}
