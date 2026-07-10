package engine

import (
	"encoding/json"
	"time"
)

func init() {
	Register(Descriptor{
		Type:     "input.manual",
		Category: "input",
		Title:    "Manual input",
		Outputs:  []Port{{Name: "out", Kind: Bool}},
		New:      func(map[string]Value) Node { return &manual{} },
	})

	// input.toggle is a held manual switch: the same externally-driven
	// boolean source as input.manual (it holds the last set level), a
	// distinct type so the editor can offer a momentary push-button
	// (input.manual, pulse) AND a held on/off switch (input.toggle) as two
	// clearly-named palette blocks. Both share the manual node - the
	// pulse-vs-hold difference is purely how the editor drives it.
	Register(Descriptor{
		Type:     "input.toggle",
		Category: "input",
		Title:    "Toggle switch",
		Outputs:  []Port{{Name: "out", Kind: Bool}},
		New:      func(map[string]Value) Node { return &manual{} },
	})

	// input.constant.{bool,float,text} emit a fixed, held value from their
	// "value" param on every tick - the simple "feed a steady 1/0, number
	// or string into the graph" source. Unlike input.manual/toggle they take
	// no external input: the value is baked into the graph, so it needs no
	// run/input plumbing (which is bool-only). One node, three types so the
	// static descriptor's output kind matches the value kind.
	registerConstant("input.constant.bool", "Constant (on/off)", Bool, BoolVal(false))
	registerConstant("input.constant.float", "Constant (number)", Float, FloatVal(0))
	registerConstant("input.constant.text", "Constant (text)", Text, TextVal(""))

	Register(Descriptor{
		Type:          "time.staircase",
		Category:      "time",
		Title:         "Staircase timer",
		Inputs:        []Port{{Name: "trig", Kind: Bool}},
		Outputs:       []Port{{Name: "q", Kind: Bool}},
		Params:        []Param{{Name: "duration", Kind: Float, Default: FloatVal(180)}},
		DelayBoundary: true,
		New: func(p map[string]Value) Node {
			return &staircase{duration: time.Duration(p["duration"].F * float64(time.Second))}
		},
	})

	Register(Descriptor{
		Type:     "logic.or",
		Category: "logic",
		Title:    "OR",
		Inputs:   []Port{{Name: "a", Kind: Bool}, {Name: "b", Kind: Bool}},
		Outputs:  []Port{{Name: "out", Kind: Bool}},
		New:      func(map[string]Value) Node { return orGate{} },
	})

	Register(Descriptor{
		Type:     "output.lamp",
		Category: "output",
		Title:    "Lamp",
		Inputs:   []Port{{Name: "set", Kind: Bool}},
		New:      func(map[string]Value) Node { return &lamp{} },
	})
}

// orGate is a stateless boolean OR (out = a || b). It is NOT a delay
// boundary, so a feedback loop closed purely through OR gates is a
// combinational cycle, while a loop that also passes a delay boundary
// (e.g. a staircase) is legal.
type orGate struct{}

func (orGate) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	out.SetBool("out", in.Bool("a") || in.Bool("b"))
}

// manual is an externally-driven boolean source. It is a placeholder
// for a real Source (doorbell, NFC, MQTT, ...): SetInput sets val and
// Eval mirrors it onto the "out" port.
type manual struct {
	val bool
}

func (n *manual) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	out.SetBool("out", n.val)
}

func (n *manual) setExternal(port string, v Value) {
	if port == "out" {
		n.val = v.B
	}
}

// registerConstant registers one input.constant.<kind> descriptor: a
// leaf source whose single "value" param (declared in the descriptor's
// kind so coerceParams types it) is emitted, held, on "out".
func registerConstant(typ, title string, kind Kind, def Value) {
	Register(Descriptor{
		Type:     typ,
		Category: "input",
		Title:    title,
		Outputs:  []Port{{Name: "out", Kind: kind}},
		Params:   []Param{{Name: "value", Kind: kind, Default: def}},
		New:      func(p map[string]Value) Node { return constant{v: p["value"]} },
	})
}

// constant is a fixed-value source: Eval writes its construction-time
// value onto "out" every tick, in the value's kind. It carries no state
// and takes no external input. It is a selfStarter so the engine
// evaluates it once at startup (no trigger would otherwise reach it).
type constant struct{ v Value }

func (constant) selfStart() {}

func (n constant) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	switch n.v.Kind {
	case Float:
		out.SetFloat("out", n.v.F)
	case Text:
		out.SetText("out", n.v.S)
	default:
		out.SetBool("out", n.v.B)
	}
}

// staircase is a retriggerable staircase-light timer. A rising edge
// on "trig" turns "q" on for duration; another rising edge restarts
// the full duration from now (so 2000 + 3000 = 5000, never 3300). It
// is a delay boundary and uses WakeAfter for the turn-off.
//
// It is Stateful: only "until" is remanent (a restart survives a
// reboot); prevTrig is live state, rebuilt from the next trig sample.
type staircase struct {
	duration time.Duration
	until    time.Time // q stays on until this logical time (zero = off)
	prevTrig bool      // last sampled trig level, for edge detection
}

func (n *staircase) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	now := ctx.Now()
	trig := in.Bool("trig")
	rising := trig && !n.prevTrig
	n.prevTrig = trig

	if rising {
		n.until = now.Add(n.duration)
		ctx.WakeAfter(n.duration)
	}

	on := !n.until.IsZero() && now.Before(n.until)
	if !on && !n.until.IsZero() {
		n.until = time.Time{} // timer expired; disarm
	}
	out.SetBool("q", on)
}

func (n *staircase) Snapshot() any {
	return staircaseState{Until: n.until}
}

func (n *staircase) Restore(data json.RawMessage) error {
	var s staircaseState
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	n.until = s.Until
	return nil
}

// staircaseState is the remanent slice of a staircase persisted by
// S1-03. prevTrig is deliberately absent - it is live state.
type staircaseState struct {
	Until time.Time `json:"until"`
}

// lamp is a boolean sink that records every state transition of its
// "set" input. It is a placeholder for a real Sink (relay, FCM,
// routing). Its known state starts off, so a leading "off" is not
// recorded as a spurious transition.
type lamp struct {
	state   bool
	changes []LampChange
}

// LampChange is one recorded on/off transition with its logical time.
type LampChange struct {
	At time.Time
	On bool
}

func (n *lamp) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	set := in.Bool("set")
	if set == n.state {
		return
	}
	n.state = set
	n.changes = append(n.changes, LampChange{At: ctx.Now(), On: set})
	ctx.Emit(Event{Node: ctx.nodeID, Type: "lamp", At: ctx.Now(), Value: BoolVal(set)})
}

// Changes returns the recorded on/off transitions in order.
func (n *lamp) Changes() []LampChange { return n.changes }
