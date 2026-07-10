package engine

import (
	"container/heap"
	"fmt"
	"strings"
	"sync"
	"time"
)

// endpoint identifies a concrete (node, port) pair.
type endpoint struct {
	node string
	port string
}

// Engine executes a validated, acyclic graph of Nodes on an
// injectable logical clock. It is single-goroutine: build the graph
// with Add/Connect, then drive it with Tick (and SetInput between
// ticks for source simulation). Nothing here touches the wall clock,
// which keeps tests deterministic and instant.
//
// The zero value is not usable; construct with New.
type Engine struct {
	tick  time.Duration
	start time.Time
	now   time.Time

	addOrder []string                // node ids in insertion order
	nodes    map[string]Node         // id -> node
	outs     map[string]storeMap     // id -> port -> last output value
	inEdge   map[string]edgeMap      // dst id -> dst port -> source endpoint
	wires    map[endpoint][]endpoint // source endpoint -> driven dst endpoints
	boundary map[string]bool         // id -> is a delay boundary (cuts feedback)

	order     []string // cached topological order of node ids
	topoStale bool     // order needs recompute
	dirty     map[string]bool

	timers timerHeap // min-heap of pending wakeups, keyed by fire time

	// pending is the async input queue: external values staged by
	// EnqueueInput (driver Source callbacks from other goroutines),
	// drained and applied at the start of the next Tick. It keeps async
	// I/O off the eval path so the engine stays single-threaded. T1
	// applies all staged events FIFO each tick; repeated writes to the
	// same port collapse to last-wins at eval (level semantics). A
	// high-rate driver wanting an enqueue bound or coalescing is a
	// follow-up.
	pending []inputEvent

	events []Event // collected Emit stub output

	// Monitor fan-out (S1-02). mu guards the fields below plus the
	// state Tick mutates, so Subscribe/Snapshot are race-safe against
	// a Tick running on another goroutine. The tick computation itself
	// is unchanged and stays deterministic.
	mu           sync.Mutex
	tickCount    int64                   // total ticks elapsed
	frameChanges []Change                // signals changed during the current tick
	subs         map[chan Frame]struct{} // monitor subscribers
}

type storeMap = map[string]Value
type edgeMap = map[string]endpoint

// New constructs an empty engine driven at the given logical tick.
// The logical clock starts at the zero time; SetStart overrides it.
func New(tick time.Duration) *Engine {
	if tick <= 0 {
		panic("engine: tick must be > 0")
	}
	e := &Engine{
		tick:     tick,
		nodes:    map[string]Node{},
		outs:     map[string]storeMap{},
		inEdge:   map[string]edgeMap{},
		wires:    map[endpoint][]endpoint{},
		boundary: map[string]bool{},
		dirty:    map[string]bool{},
		subs:     map[chan Frame]struct{}{},
	}
	heap.Init(&e.timers)
	return e
}

// SetStart sets the logical start time. Call before the first Tick.
func (e *Engine) SetStart(t time.Time) {
	e.start = t
	e.now = t
}

// Now returns the current logical time.
func (e *Engine) Now() time.Time { return e.now }

// Elapsed returns logical time since the start.
func (e *Engine) Elapsed() time.Duration { return e.now.Sub(e.start) }

// Events returns the events emitted so far (Emit is a stub in S1-01).
func (e *Engine) Events() []Event { return e.events }

// Add inserts a node under the given id. It panics on a duplicate id.
func (e *Engine) Add(id string, n Node) {
	if _, dup := e.nodes[id]; dup {
		panic("engine: duplicate node id " + id)
	}
	e.nodes[id] = n
	e.outs[id] = storeMap{}
	e.addOrder = append(e.addOrder, id)
	e.topoStale = true
	// A self-starting source (e.g. input.constant) has no external trigger
	// and no upstream, so mark it dirty at insertion time: the first Tick
	// then evaluates it once and its held value settles (and propagates)
	// like any other change. Nodes without the marker stay event-driven.
	if _, ok := n.(selfStarter); ok {
		e.markDirty(id)
	}
}

// AddType constructs a node from the registry and adds it under id,
// returning the constructed node so callers can hold a reference.
func (e *Engine) AddType(id, typ string, params map[string]Value) (Node, error) {
	n, err := Construct(typ, params)
	if err != nil {
		return nil, err
	}
	e.Add(id, n)
	if d, ok := Lookup(typ); ok {
		e.boundary[id] = d.DelayBoundary
	}
	return n, nil
}

// Connect wires one output port to one input port. Each input port
// may be driven by at most one edge; a second Connect to the same
// input panics. Both nodes must already exist.
func (e *Engine) Connect(srcNode, srcPort, dstNode, dstPort string) {
	if _, ok := e.nodes[srcNode]; !ok {
		panic("engine: Connect from unknown node " + srcNode)
	}
	if _, ok := e.nodes[dstNode]; !ok {
		panic("engine: Connect to unknown node " + dstNode)
	}
	edges := e.inEdge[dstNode]
	if edges == nil {
		edges = edgeMap{}
		e.inEdge[dstNode] = edges
	}
	if _, exists := edges[dstPort]; exists {
		panic(fmt.Sprintf("engine: input port %s.%s already driven", dstNode, dstPort))
	}
	src := endpoint{srcNode, srcPort}
	edges[dstPort] = src
	e.wires[src] = append(e.wires[src], endpoint{dstNode, dstPort})
	e.topoStale = true
}

// inputEvent is one staged external input in the async tick queue.
type inputEvent struct {
	node string
	port string
	v    Value
}

// SetInput injects an external value into a source node and marks it
// dirty, simulating a real Source (doorbell, NFC, MQTT, ...). The node
// must accept external input (e.g. input.manual). It applies the value
// immediately (under e.mu); the effect is observed on the next Tick,
// exactly as before. Driver callbacks from other goroutines use
// EnqueueInput instead.
func (e *Engine) SetInput(nodeID, port string, v Value) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applyExternal(nodeID, port, v)
}

// EnqueueInput stages an external value to be applied at the start of the
// next Tick. It is the async entry point for driver Source callbacks:
// safe to call from any goroutine, it validates and appends under e.mu
// and never evaluates. The value reaches the graph through the dirty set
// on the next tick - off the eval path - so the engine stays
// single-threaded and deterministic.
func (e *Engine) EnqueueInput(nodeID, port string, v Value) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, ok := e.nodes[nodeID]
	if !ok {
		panic("engine: EnqueueInput on unknown node " + nodeID)
	}
	if _, ok := n.(externalSetter); !ok {
		panic("engine: node " + nodeID + " does not accept external input")
	}
	e.pending = append(e.pending, inputEvent{node: nodeID, port: port, v: v})
}

// applyExternal applies one external value to a source node and marks it
// dirty. The caller must hold e.mu. It is the shared apply path behind
// the synchronous SetInput and the drained async EnqueueInput.
func (e *Engine) applyExternal(nodeID, port string, v Value) {
	n, ok := e.nodes[nodeID]
	if !ok {
		panic("engine: external input on unknown node " + nodeID)
	}
	if es, ok := n.(externalSetter); ok {
		es.setExternal(port, v)
	} else {
		panic("engine: node " + nodeID + " does not accept external input")
	}
	e.markDirty(nodeID)
}

// externalSetter is implemented by source nodes that can be driven
// from outside the graph via Engine.SetInput.
type externalSetter interface {
	setExternal(port string, v Value)
}

// selfStarter is implemented by nodes that must be evaluated once at
// startup even without an external trigger or upstream change, so their
// held output is established on the first Tick (e.g. input.constant).
// Add marks them dirty at insertion time; the marker method is never
// called - its presence is the whole contract.
type selfStarter interface{ selfStart() }

func (e *Engine) markDirty(id string) { e.dirty[id] = true }

// Tick advances the logical clock by one tick and settles the graph:
//
//  1. now += tick
//  2. fire every wakeup that is now due, marking its node dirty
//  3. evaluate the dirty set in topological order; clearing each
//     node's dirty flag before Eval
//  4. an output that actually changes marks its downstream consumers
//     dirty; they sit later in the topo order and so are caught in
//     this same pass
//
// After the pass, the signals that actually changed this tick are
// fanned out to monitor subscribers as one Frame (empty ticks send
// nothing). The fan-out is non-blocking and never affects the tick.
func (e *Engine) Tick() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.tickCount++
	e.now = e.now.Add(e.tick)
	e.frameChanges = nil

	// Drain the async input queue at the tick boundary, before anything
	// evaluates: each staged driver value is applied (sets the source's
	// value and marks it dirty) so it reaches eval only through this
	// tick's dirty set, never concurrently with the topo walk. Drain and
	// the timer pop both only stage dirtiness; evaluation is purely
	// topological over the order-insensitive dirty set, so the trace is
	// the same regardless of which marks a node dirty first.
	if len(e.pending) > 0 {
		for _, ev := range e.pending {
			e.applyExternal(ev.node, ev.port, ev.v)
		}
		e.pending = e.pending[:0]
	}

	for e.timers.Len() > 0 && !e.timers[0].at.After(e.now) {
		w := heap.Pop(&e.timers).(wakeup)
		e.markDirty(w.node)
	}

	if len(e.dirty) > 0 {
		for _, id := range e.topo() {
			if !e.dirty[id] {
				continue
			}
			delete(e.dirty, id)
			e.evalNode(id)
		}
	}

	if len(e.frameChanges) > 0 {
		e.fanout(Frame{
			Tick:    e.tickCount,
			TimeMs:  e.now.Sub(e.start).Milliseconds(),
			Changes: e.frameChanges,
		})
		e.frameChanges = nil // handed off to the Frame; next tick allocates fresh
	}
}

// evalNode runs one node's Eval, then propagates any changed outputs
// along their wires: each downstream input is marked dirty for this
// same pass and recorded as a Change on the current tick's frame.
//
// Signals are reported at the consuming (destination) end of each
// wire - that is what the editor highlights as a live value - so a
// changed staircase "q" surfaces as a change on lamp "set".
func (e *Engine) evalNode(id string) {
	var changed []string
	in := inAdapter{eng: e, node: id}
	out := &outAdapter{eng: e, node: id, changed: &changed}
	ctx := &EvalContext{eng: e, nodeID: id}

	e.nodes[id].Eval(ctx, in, out)

	for _, port := range changed {
		v := e.outs[id][port]
		// Report the value at the node's OWN output port too, so a source's
		// value is observable on its card even with no downstream wire (an
		// unconnected telemetry source must still show its number). The
		// destination changes below remain what drives wire/lit rendering.
		e.frameChanges = append(e.frameChanges, Change{Node: id, Port: port, Value: v})
		for _, dst := range e.wires[endpoint{id, port}] {
			e.markDirty(dst.node)
			e.frameChanges = append(e.frameChanges, Change{Node: dst.node, Port: dst.port, Value: v})
		}
	}
}

// topo returns the cached evaluation order, recomputing it with the
// shared delay-boundary cut (see topoCut) when the graph changed. A
// residual combinational cycle panics: it violates the kernel's
// precondition, which Build/Validate guarantee for any graph that
// reaches the engine. A legal feedback loop through a delay boundary
// does not panic - the cut breaks it.
func (e *Engine) topo() []string {
	if !e.topoStale {
		return e.order
	}

	deps := make([]depEdge, 0, len(e.inEdge))
	for dst, edges := range e.inEdge {
		for dstPort, src := range edges {
			deps = append(deps, depEdge{src: src.node, srcPort: src.port, dst: dst, dstPort: dstPort})
		}
	}

	order, cyclic := topoCut(e.addOrder, deps, func(id string) bool { return e.boundary[id] })
	if len(cyclic) > 0 {
		panic("engine: combinational cycle through " + strings.Join(cyclic, ", ") +
			" - kernel requires a validated graph")
	}

	e.order = order
	e.topoStale = false
	return e.order
}

// --- input adapter ---------------------------------------------------

// inAdapter reads a node's input ports out of the upstream nodes'
// output stores.
type inAdapter struct {
	eng  *Engine
	node string
}

func (a inAdapter) value(port string) (Value, bool) {
	ep, ok := a.eng.inEdge[a.node][port]
	if !ok {
		return Value{}, false
	}
	return a.eng.outs[ep.node][ep.port], true
}

func (a inAdapter) Bool(port string) bool      { v, _ := a.value(port); return v.B }
func (a inAdapter) Float(port string) float64  { v, _ := a.value(port); return v.F }
func (a inAdapter) Text(port string) string    { v, _ := a.value(port); return v.S }
func (a inAdapter) Connected(port string) bool { _, ok := a.value(port); return ok }

// --- output adapter --------------------------------------------------

// outAdapter writes a node's output ports into its store, recording
// which ports actually changed so the engine can propagate dirtiness.
type outAdapter struct {
	eng     *Engine
	node    string
	changed *[]string
}

func (a *outAdapter) set(port string, v Value) {
	store := a.eng.outs[a.node]
	if old, ok := store[port]; ok && old == v {
		return // unchanged: no write-through, no propagation
	}
	store[port] = v
	*a.changed = append(*a.changed, port)
}

func (a *outAdapter) SetBool(port string, v bool)     { a.set(port, Value{Kind: Bool, B: v}) }
func (a *outAdapter) SetFloat(port string, v float64) { a.set(port, Value{Kind: Float, F: v}) }
func (a *outAdapter) SetText(port string, v string)   { a.set(port, Value{Kind: Text, S: v}) }

// --- evaluation context ----------------------------------------------

// EvalContext is the per-node handle into the engine during Eval. It
// exposes the logical clock, scheduling, and the (stub) event sink.
type EvalContext struct {
	eng    *Engine
	nodeID string
}

// Now returns the current logical time.
func (c *EvalContext) Now() time.Time { return c.eng.now }

// WakeAfter schedules this node to be re-evaluated once the logical
// clock reaches now+d. Multiple wakeups may be outstanding for a
// node; each just marks it dirty when it fires.
func (c *EvalContext) WakeAfter(d time.Duration) {
	heap.Push(&c.eng.timers, wakeup{at: c.eng.now.Add(d), node: c.nodeID})
}

// Emit records an event. In S1-01 this is a collecting stub (see
// Engine.Events); a later ticket routes events to a persistent sink.
func (c *EvalContext) Emit(ev Event) {
	c.eng.events = append(c.eng.events, ev)
}

// --- timer heap ------------------------------------------------------

// wakeup is one scheduled re-evaluation of a node.
type wakeup struct {
	at   time.Time
	node string
}

// timerHeap is a min-heap of wakeups ordered by fire time, backing
// the engine's WakeAfter scheduling via container/heap.
type timerHeap []wakeup

func (h timerHeap) Len() int           { return len(h) }
func (h timerHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h timerHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *timerHeap) Push(x any)        { *h = append(*h, x.(wakeup)) }
func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	w := old[n-1]
	*h = old[:n-1]
	return w
}
