package engine

// Generic I/O nodes that bind a graph endpoint to a driver channel via
// the binding table (see BindGraph). They are the engine-side seam for
// real hardware: input.manual and output.lamp stay untouched (the demo
// and editor still use them), while a graph that talks to a driver uses
// these. Each value kind is its own node TYPE (Bool / Float / Text), so
// the static descriptor's port kind matches the channel kind BindGraph
// validates; one shared implementation is parameterised by kind. The
// async input path (EnqueueInput / applyExternal / the tick queue)
// already carries a typed Value, so opening the nodes to Float/Text needs
// no change there - and the Bool nodes stay byte-identical.
const (
	TypeSourceChannel      = "source.channel"
	TypeSourceChannelFloat = "source.channel.float"
	TypeSourceChannelText  = "source.channel.text"
	TypeSinkChannel        = "sink.channel"
	TypeSinkChannelFloat   = "sink.channel.float"
	TypeSinkChannelText    = "sink.channel.text"
)

func init() {
	registerSourceChannel(TypeSourceChannel, "Channel input", Bool)
	registerSourceChannel(TypeSourceChannelFloat, "Channel input (float)", Float)
	registerSourceChannel(TypeSourceChannelText, "Channel input (text)", Text)
	registerSinkChannel(TypeSinkChannel, "Channel output", Bool)
	registerSinkChannel(TypeSinkChannelFloat, "Channel output (float)", Float)
	registerSinkChannel(TypeSinkChannelText, "Channel output (text)", Text)
}

func registerSourceChannel(typ, title string, kind Kind) {
	Register(Descriptor{
		Type:     typ,
		Category: "input",
		Title:    title,
		Outputs:  []Port{{Name: "out", Kind: kind}},
		Params:   []Param{{Name: "channel", Kind: Text, Required: true}},
		New:      func(map[string]Value) Node { return &sourceChannel{kind: kind} },
	})
}

func registerSinkChannel(typ, title string, kind Kind) {
	Register(Descriptor{
		Type:     typ,
		Category: "output",
		Title:    title,
		Inputs:   []Port{{Name: "in", Kind: kind}},
		Params:   []Param{{Name: "channel", Kind: Text, Required: true}},
		New:      func(map[string]Value) Node { return &sinkChannel{kind: kind, state: zeroOf(kind)} },
	})
}

// zeroOf is the zero Value of a kind. A sink seeds its known state with it
// so a leading zero-of-kind is not a spurious write - the same "starts
// off" guarantee the Bool sink had, now correct for Float and Text too
// (without it the zero state would carry Kind Bool and never compare equal
// to a Float/Text input).
func zeroOf(k Kind) Value {
	switch k {
	case Float:
		return FloatVal(0)
	case Text:
		return TextVal("")
	default:
		return BoolVal(false)
	}
}

// sourceChannel is a generic external input bound to a driver Source via
// BindGraph. Like input.manual it is an externalSetter: the engine's
// async queue (EnqueueInput) stages a value that the tick drain mirrors
// onto val, and Eval writes val to "out" in the node's kind. It is a leaf
// and not a delay boundary. The "channel" param is the logical name the
// table resolves; the node itself is channel-agnostic (BindGraph wires it).
type sourceChannel struct {
	kind Kind
	val  Value
}

func (n *sourceChannel) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	switch n.kind {
	case Float:
		out.SetFloat("out", n.val.F)
	case Text:
		out.SetText("out", n.val.S)
	default:
		out.SetBool("out", n.val.B)
	}
}

func (n *sourceChannel) setExternal(port string, v Value) {
	if port == "out" {
		n.val = v
	}
}

// sinkChannel is a generic output bound to a driver Sink via BindGraph.
// Like output.lamp it records only transitions - its known state starts at
// the zero of its kind, so a leading zero is not a spurious write - and on
// each change calls onWrite (the bound Sink.Write) plus Emit. onWrite is
// set by BindGraph after Build; an unbound sink still evaluates (onWrite
// nil). onWrite runs synchronously inside Eval under the engine lock, so
// the Sink contract is non-blocking and must not re-enter the engine.
type sinkChannel struct {
	kind    Kind
	state   Value
	onWrite func(Value)
}

func (n *sinkChannel) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	var v Value
	switch n.kind {
	case Float:
		v = FloatVal(in.Float("in"))
	case Text:
		v = TextVal(in.Text("in"))
	default:
		v = BoolVal(in.Bool("in"))
	}
	if v == n.state {
		return
	}
	n.state = v
	if n.onWrite != nil {
		n.onWrite(v)
	}
	ctx.Emit(Event{Node: ctx.nodeID, Type: "sink", At: ctx.Now(), Value: v})
}
