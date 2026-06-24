package engine

import (
	"container/heap"
	"fmt"
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

	order     []string // cached topological order of node ids
	topoStale bool     // order needs recompute
	dirty     map[string]bool

	timers timerHeap // min-heap of pending wakeups, keyed by fire time

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
		tick:   tick,
		nodes:  map[string]Node{},
		outs:   map[string]storeMap{},
		inEdge: map[string]edgeMap{},
		wires:  map[endpoint][]endpoint{},
		dirty:  map[string]bool{},
		subs:   map[chan Frame]struct{}{},
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
}

// AddType constructs a node from the registry and adds it under id,
// returning the constructed node so callers can hold a reference.
func (e *Engine) AddType(id, typ string, params map[string]Value) (Node, error) {
	n, err := Construct(typ, params)
	if err != nil {
		return nil, err
	}
	e.Add(id, n)
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

// SetInput injects an external value into a source node and marks it
// dirty, simulating a real Source (doorbell, NFC, MQTT, ...). The
// node must accept external input (e.g. input.manual).
func (e *Engine) SetInput(nodeID, port string, v Value) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, ok := e.nodes[nodeID]
	if !ok {
		panic("engine: SetInput on unknown node " + nodeID)
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
		for _, dst := range e.wires[endpoint{id, port}] {
			e.markDirty(dst.node)
			e.frameChanges = append(e.frameChanges, Change{Node: dst.node, Port: dst.port, Value: v})
		}
	}
}

// topo returns the cached topological order, recomputing it via
// Kahn's algorithm when the graph changed. The graph is assumed
// acyclic (the validator's job); a residual cycle panics, since it
// violates the kernel's precondition.
func (e *Engine) topo() []string {
	if !e.topoStale {
		return e.order
	}

	indeg := make(map[string]int, len(e.nodes))
	adj := make(map[string]map[string]bool, len(e.nodes))
	for id := range e.nodes {
		indeg[id] = 0
	}
	for dst, edges := range e.inEdge {
		for _, src := range edges {
			set := adj[src.node]
			if set == nil {
				set = map[string]bool{}
				adj[src.node] = set
			}
			if !set[dst] { // collapse parallel edges to one dependency
				set[dst] = true
				indeg[dst]++
			}
		}
	}

	// Seed the queue in insertion order for a deterministic result.
	queue := make([]string, 0, len(e.nodes))
	for _, id := range e.addOrder {
		if indeg[id] == 0 {
			queue = append(queue, id)
		}
	}

	order := make([]string, 0, len(e.nodes))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for dst := range adj[id] {
			indeg[dst]--
			if indeg[dst] == 0 {
				queue = append(queue, dst)
			}
		}
	}

	if len(order) != len(e.nodes) {
		panic("engine: graph has a cycle - kernel requires a validated acyclic graph")
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
