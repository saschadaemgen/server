// Package engine is the headless execution core of the CARVILON
// logic engine - the local-first equivalent of a Loxone Miniserver.
// A (later) visual editor produces graphs of building blocks
// (AND, timer, staircase light, relay, ...) and this package runs
// them server-side on the edge (RPi up to a Linux box).
//
// ENGINE-S1-01 builds only the execution kernel: no editor, no
// graph-JSON parsing, no database, no hardware. It proves that the
// kernel lives by running a 3-node graph
//
//	input.manual -> time.staircase(3s) -> output.lamp
//
// on an injectable logical clock and reproducing the authoritative
// trace (see engine_test.go):
//
//	button pressed at 300ms   -> lamp ON  at 300ms
//	re-trigger     at 2000ms  -> stays ON
//	                          -> lamp OFF at 5000ms  (2000+3000, NOT 3300)
//
// Out of scope here (later tickets): the editor and catalog.json
// endpoint (uses the registry built here), validate / cycle &
// delay-boundary checking (S1-02 - the kernel assumes a validated
// acyclic graph), persistence / snapshot restore (S1-03 - the
// Stateful interface is defined here), real Source/Sink drivers,
// and a persistent Emit sink (Emit is a collecting stub here).
package engine

import (
	"encoding/json"
	"time"
)

// Kind is the value domain a port or parameter carries.
type Kind uint8

const (
	// Bool is a boolean value (digital signal).
	Bool Kind = iota
	// Float is a 64-bit floating point value (analog signal).
	Float
	// Text is a string value.
	Text
)

// Value is a tagged union over the three supported kinds. Only the
// field matching Kind is meaningful; the others are zero. Value is
// comparable (==), which the engine relies on for change detection.
type Value struct {
	Kind Kind
	B    bool
	F    float64
	S    string
}

// BoolVal, FloatVal and TextVal are convenience constructors.
func BoolVal(v bool) Value     { return Value{Kind: Bool, B: v} }
func FloatVal(v float64) Value { return Value{Kind: Float, F: v} }
func TextVal(v string) Value   { return Value{Kind: Text, S: v} }

// Inputs is the read side a Node sees during Eval. A port reads the
// value of whatever output drives it; an unconnected port reads the
// zero value of its kind. Connected reports whether an edge feeds
// the port at all.
type Inputs interface {
	Bool(port string) bool
	Float(port string) float64
	Text(port string) string
	Connected(port string) bool
}

// Outputs is the write side a Node sees during Eval. A write only
// propagates downstream when the value actually changes (the engine
// compares against the last value written to that port).
type Outputs interface {
	SetBool(port string, v bool)
	SetFloat(port string, v float64)
	SetText(port string, v string)
}

// Node is one building block instance. Eval is called whenever the
// node is dirty (external input changed, a wake fired, or an
// upstream output changed). A Node MUST be deterministic in (in,
// ctx.Now(), internal state) and MUST NOT block or sleep - timing
// is expressed through ctx.WakeAfter.
type Node interface {
	Eval(ctx *EvalContext, in Inputs, out Outputs)
}

// Stateful is implemented by nodes that carry remanent state across
// restarts. S1-03 persists Snapshot() and replays it via Restore().
// Only durable state belongs here; live/derivable state (such as a
// previous-edge flag) is rebuilt on the first ticks after restore.
type Stateful interface {
	Snapshot() any
	Restore(data json.RawMessage) error
}

// Event is one observation emitted by a node through ctx.Emit. In
// S1-01 events are merely collected in-memory (see Engine.Events);
// a later ticket routes them to history / push / the event bus.
type Event struct {
	Node  string    // id of the emitting node
	Type  string    // event kind, e.g. "lamp"
	At    time.Time // logical time of emission
	Value Value     // payload
}
