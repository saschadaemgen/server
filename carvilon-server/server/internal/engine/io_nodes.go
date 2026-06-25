package engine

// Generic I/O nodes that bind a graph endpoint to a driver channel via
// the binding table (see BindGraph). They are the engine-side seam for
// real hardware: input.manual and output.lamp stay untouched (the demo
// and editor still use them), while a graph that talks to a driver uses
// these. T1 carries Bool only; Float/Text channels are a follow-up.
const (
	TypeSourceChannel = "source.channel"
	TypeSinkChannel   = "sink.channel"
)

func init() {
	Register(Descriptor{
		Type:     TypeSourceChannel,
		Category: "input",
		Title:    "Channel input",
		Outputs:  []Port{{Name: "out", Kind: Bool}},
		Params:   []Param{{Name: "channel", Kind: Text, Required: true}},
		New:      func(map[string]Value) Node { return &sourceChannel{} },
	})
	Register(Descriptor{
		Type:     TypeSinkChannel,
		Category: "output",
		Title:    "Channel output",
		Inputs:   []Port{{Name: "in", Kind: Bool}},
		Params:   []Param{{Name: "channel", Kind: Text, Required: true}},
		New:      func(map[string]Value) Node { return &sinkChannel{} },
	})
}

// sourceChannel is a generic external input bound to a driver Source via
// BindGraph. Like input.manual it is an externalSetter: the engine's
// async queue (EnqueueInput) stages a value that the tick drain mirrors
// onto val, and Eval writes val to "out". It is a leaf and not a delay
// boundary. The "channel" param is the logical name the table resolves;
// the node itself is channel-agnostic (BindGraph does the wiring).
type sourceChannel struct {
	val Value
}

func (n *sourceChannel) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	out.SetBool("out", n.val.B) // T1: Bool channel
}

func (n *sourceChannel) setExternal(port string, v Value) {
	if port == "out" {
		n.val = v
	}
}

// sinkChannel is a generic output bound to a driver Sink via BindGraph.
// Like output.lamp it records only transitions - its known state starts
// off, so a leading off is not a spurious write - and on each change
// calls onWrite (the bound Sink.Write) plus Emit. onWrite is set by
// BindGraph after Build; an unbound sink still evaluates (onWrite nil).
// onWrite runs synchronously inside Eval under the engine lock, so the
// Sink contract is non-blocking and must not re-enter the engine.
type sinkChannel struct {
	state   Value
	onWrite func(Value)
}

func (n *sinkChannel) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	v := BoolVal(in.Bool("in")) // T1: Bool channel
	if v == n.state {
		return
	}
	n.state = v
	if n.onWrite != nil {
		n.onWrite(v)
	}
	ctx.Emit(Event{Node: ctx.nodeID, Type: "sink", At: ctx.Now(), Value: v})
}
