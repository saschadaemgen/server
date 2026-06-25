package engine

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildAdapterGraph wires source.channel -> time.staircase(3s) ->
// sink.channel and binds both ends to a fresh virtual driver under the
// "virtual" prefix. It is the canonical staircase graph reached entirely
// through the adapter layer (the logical channels "door"/"relay" resolve
// to virtual addresses "btn0"/"lamp0").
func buildAdapterGraph(t *testing.T, tick time.Duration) (*Engine, *VirtualDriver) {
	t.Helper()
	g := Graph{
		Schema: SchemaVersion,
		Nodes: []GraphNode{
			{ID: "src", Type: TypeSourceChannel, Params: map[string]any{"channel": "door"}},
			{ID: "stair", Type: "time.staircase", Params: map[string]any{"duration": 3.0}},
			{ID: "snk", Type: TypeSinkChannel, Params: map[string]any{"channel": "relay"}},
		},
		Edges: []GraphEdge{
			{From: "src:out", To: "stair:trig"},
			{From: "stair:q", To: "snk:in"},
		},
	}
	eng, err := Build(g, DefaultRegistry(), tick)
	if err != nil {
		t.Fatalf("build adapter graph: %v", err)
	}
	vd := NewVirtualDriver(
		Channel{Address: "btn0", Label: "button", Kind: Bool},
		Channel{Address: "lamp0", Label: "relay", Kind: Bool},
	)
	reg := NewDriverRegistry()
	reg.RegisterSource(PrefixVirtual, vd)
	reg.RegisterSink(PrefixVirtual, vd)
	table := BindingTable{
		"door":  {Prefix: PrefixVirtual, Addr: "btn0"},
		"relay": {Prefix: PrefixVirtual, Addr: "lamp0"},
	}
	if err := BindGraph(eng, g, table, nil, reg); err != nil {
		t.Fatalf("bind adapter graph: %v", err)
	}
	return eng, vd
}

// TestAdapterStaircaseTrace proves the full path async-event ->
// EnqueueInput -> tick-queue drain -> eval -> staircase -> sink onWrite
// -> Sink.Write. The virtual source drives the exact schedule of
// TestStaircaseTrace and the virtual sink records the authoritative trace
// (on@300ms, off@5000ms, never@3300ms), with the staircase in the middle
// computing unchanged.
func TestAdapterStaircaseTrace(t *testing.T) {
	const tick = 100 * time.Millisecond
	eng, vd := buildAdapterGraph(t, tick)

	var onTick, offTick, writes int
	for i := 1; i <= 60; i++ {
		switch time.Duration(i) * tick {
		case 300 * time.Millisecond:
			vd.SetSource("btn0", BoolVal(true))
		case 1000 * time.Millisecond:
			vd.SetSource("btn0", BoolVal(false))
		case 2000 * time.Millisecond:
			vd.SetSource("btn0", BoolVal(true)) // re-trigger
		}
		before := len(vd.SinkWrites("lamp0"))
		eng.Tick()
		w := vd.SinkWrites("lamp0")
		if len(w) > before {
			writes++
			if w[len(w)-1].B {
				onTick = i
			} else {
				offTick = i
			}
		}
	}

	if writes != 2 {
		t.Fatalf("sink got %d writes, want exactly 2: %+v", writes, vd.SinkWrites("lamp0"))
	}
	if onTick != 3 {
		t.Errorf("sink turned on at tick %d (%v); want 300ms", onTick, time.Duration(onTick)*tick)
	}
	if offTick != 50 {
		t.Errorf("sink turned off at tick %d (%v); want 5000ms", offTick, time.Duration(offTick)*tick)
	}
	if onTick == 33 || offTick == 33 {
		t.Errorf("sink switched at 3300ms; the re-trigger must extend the hold to 5000ms")
	}
	if w := vd.SinkWrites("lamp0"); !w[0].B || w[1].B {
		t.Errorf("sink write values = %+v; want [true,false]", w)
	}
}

// TestAdapterAsyncCallbackLandsNextTick proves the async->tick-queue
// contract: a Source callback fired from a real goroutine only stages the
// value (nothing evaluates), and exactly one Tick applies it end to end.
// Run under -race, it also guards that the async path never touches eval.
func TestAdapterAsyncCallbackLandsNextTick(t *testing.T) {
	const tick = 100 * time.Millisecond
	eng, vd := buildAdapterGraph(t, tick)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		vd.SetSource("btn0", BoolVal(true)) // from another goroutine
	}()
	wg.Wait()

	// Staged only - prove the deferral concretely: the value sits in the
	// tick queue, the source node has NOT been mutated, and nothing has
	// evaluated (the sink has not written).
	if got := len(eng.pending); got != 1 {
		t.Fatalf("expected 1 value staged in the tick queue, got %d", got)
	}
	if eng.nodes["src"].(*sourceChannel).val.B {
		t.Fatalf("async value reached the source node before any Tick; it must defer to the next tick")
	}
	if w := vd.SinkWrites("lamp0"); len(w) != 0 {
		t.Fatalf("sink wrote before any Tick: %+v", w)
	}

	eng.Tick() // one tick drains the queue and settles the whole graph

	if got := len(eng.pending); got != 0 {
		t.Errorf("tick queue not drained after one Tick: %d left", got)
	}
	if w := vd.SinkWrites("lamp0"); len(w) != 1 || !w[0].B {
		t.Fatalf("after one Tick, sink writes = %+v; want [true]", w)
	}
}

// TestAdapterDeterminism proves the same input sequence yields the same
// output sequence (the engine's logical-clock determinism, preserved
// across the adapter layer).
func TestAdapterDeterminism(t *testing.T) {
	const tick = 100 * time.Millisecond
	run := func() []Value {
		eng, vd := buildAdapterGraph(t, tick)
		for i := 1; i <= 60; i++ {
			switch time.Duration(i) * tick {
			case 300 * time.Millisecond:
				vd.SetSource("btn0", BoolVal(true))
			case 1000 * time.Millisecond:
				vd.SetSource("btn0", BoolVal(false))
			case 2000 * time.Millisecond:
				vd.SetSource("btn0", BoolVal(true))
			}
			eng.Tick()
		}
		return vd.SinkWrites("lamp0")
	}
	a, b := run(), run()
	if !reflect.DeepEqual(a, b) {
		t.Errorf("non-deterministic sink writes: %+v vs %+v", a, b)
	}
	if len(a) != 2 {
		t.Errorf("expected 2 sink writes, got %d: %+v", len(a), a)
	}
}

// TestAdapterCoalescing proves level semantics: several source edges with
// no Tick between collapse to the final level when the queue drains.
func TestAdapterCoalescing(t *testing.T) {
	const tick = 100 * time.Millisecond

	// Final level true survives -> exactly one sink write [true], not one
	// per edge.
	eng, vd := buildAdapterGraph(t, tick)
	vd.SetSource("btn0", BoolVal(true))
	vd.SetSource("btn0", BoolVal(false))
	vd.SetSource("btn0", BoolVal(true)) // final level true
	eng.Tick()
	if w := vd.SinkWrites("lamp0"); len(w) != 1 || !w[0].B {
		t.Fatalf("true,false,true coalesced = %+v; want [true]", w)
	}

	// Last-wins, NOT any-true-wins: a true overwritten by false before the
	// tick must never reach eval, so the intermediate true does not fire
	// the staircase and the sink stays silent. (If the queue applied
	// each edge to eval, the intermediate true would have produced an on
	// write.)
	eng2, vd2 := buildAdapterGraph(t, tick)
	vd2.SetSource("btn0", BoolVal(true))
	vd2.SetSource("btn0", BoolVal(false)) // final level false
	eng2.Tick()
	if w := vd2.SinkWrites("lamp0"); len(w) != 0 {
		t.Fatalf("true,false coalesced fired the sink = %+v; last-wins false must not trigger", w)
	}
}

// TestBindReservedPrefixRejected proves the reserved prefixes are an
// inactive-but-loud seam: binding to gpio: with no driver registered
// fails with a clear error, not a panic or a silent no-op.
func TestBindReservedPrefixRejected(t *testing.T) {
	const tick = 100 * time.Millisecond
	g := Graph{
		Schema: SchemaVersion,
		Nodes: []GraphNode{
			{ID: "src", Type: TypeSourceChannel, Params: map[string]any{"channel": "door"}},
			{ID: "snk", Type: TypeSinkChannel, Params: map[string]any{"channel": "relay"}},
		},
		Edges: []GraphEdge{{From: "src:out", To: "snk:in"}},
	}
	eng, err := Build(g, DefaultRegistry(), tick)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	reg := NewDriverRegistry() // deliberately no gpio driver
	table := BindingTable{
		"door":  {Prefix: PrefixGPIO, Addr: "17"},
		"relay": {Prefix: PrefixVirtual, Addr: "lamp0"},
	}
	err = BindGraph(eng, g, table, nil, reg)
	if err == nil {
		t.Fatalf("BindGraph must error on a reserved prefix with no driver")
	}
	if !strings.Contains(err.Error(), PrefixGPIO) {
		t.Errorf("error should name the %q prefix, got: %v", PrefixGPIO, err)
	}
}

// configurableVD is a VirtualDriver that also records the per-line options
// BindGraph hands it, to prove the config seam routes them to the right
// address and direction - and that a bound graph stays deterministic
// regardless of what options ride along.
type configurableVD struct {
	*VirtualDriver
	inCfg  map[string]ChannelConfig
	outCfg map[string]ChannelConfig
}

func newConfigurableVD(chans ...Channel) *configurableVD {
	return &configurableVD{
		VirtualDriver: NewVirtualDriver(chans...),
		inCfg:         map[string]ChannelConfig{},
		outCfg:        map[string]ChannelConfig{},
	}
}

func (d *configurableVD) ConfigureInput(addr string, cfg ChannelConfig) error {
	d.inCfg[addr] = cfg
	return nil
}

func (d *configurableVD) ConfigureOutput(addr string, cfg ChannelConfig) error {
	d.outCfg[addr] = cfg
	return nil
}

// TestBindGraphRoutesChannelConfig proves BindGraph carries each logical
// channel's options to the driver: ConfigureInput for the source line,
// ConfigureOutput for the sink line, keyed by the PHYSICAL address and
// never crossed - and that the bound graph still evaluates deterministically.
func TestBindGraphRoutesChannelConfig(t *testing.T) {
	g := Graph{
		Schema: SchemaVersion,
		Nodes: []GraphNode{
			{ID: "src", Type: TypeSourceChannel, Params: map[string]any{"channel": "door"}},
			{ID: "snk", Type: TypeSinkChannel, Params: map[string]any{"channel": "relay"}},
		},
		Edges: []GraphEdge{{From: "src:out", To: "snk:in"}},
	}
	eng, err := Build(g, DefaultRegistry(), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	vd := newConfigurableVD(
		Channel{Address: "btn0", Label: "button", Kind: Bool},
		Channel{Address: "lamp0", Label: "relay", Kind: Bool},
	)
	reg := NewDriverRegistry()
	reg.RegisterSource(PrefixVirtual, vd)
	reg.RegisterSink(PrefixVirtual, vd)
	table := BindingTable{
		"door":  {Prefix: PrefixVirtual, Addr: "btn0"},
		"relay": {Prefix: PrefixVirtual, Addr: "lamp0"},
	}
	configs := map[string]ChannelConfig{
		"door":  {"bias": "pulldown", "active_level": "high"},
		"relay": {"initial": "high"},
	}
	if err := BindGraph(eng, g, table, configs, reg); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Input options went to ConfigureInput, keyed by the physical address.
	if got := vd.inCfg["btn0"]; got["bias"] != "pulldown" || got["active_level"] != "high" {
		t.Errorf("ConfigureInput(btn0) = %v, want bias=pulldown active_level=high", got)
	}
	if _, leaked := vd.outCfg["btn0"]; leaked {
		t.Errorf("input line btn0 must not be configured as an output")
	}
	// Output options went to ConfigureOutput, not crossed into the input map.
	if got := vd.outCfg["lamp0"]; got["initial"] != "high" {
		t.Errorf("ConfigureOutput(lamp0) = %v, want initial=high", got)
	}
	if _, leaked := vd.inCfg["lamp0"]; leaked {
		t.Errorf("output line lamp0 must not be configured as an input")
	}

	// Determinism: the config is advisory to the driver; the engine still
	// passes each input edge straight through to the sink.
	vd.SetSource("btn0", BoolVal(true))
	eng.Tick()
	vd.SetSource("btn0", BoolVal(false))
	eng.Tick()
	if got := vd.SinkWrites("lamp0"); len(got) != 2 || !got[0].B || got[1].B {
		t.Errorf("sink writes = %v, want [true false]", got)
	}
}

// buildKindGraph wires source.channel.<kind> -> sink.channel.<kind> direct,
// bound to a fresh virtual driver, for a typed end-to-end path test. The
// virtual driver is already kind-agnostic (it carries Value), so the same
// helper backs Float and Text.
func buildKindGraph(t *testing.T, srcType, snkType string, kind Kind, tick time.Duration) (*Engine, *VirtualDriver) {
	t.Helper()
	g := Graph{
		Schema: SchemaVersion,
		Nodes: []GraphNode{
			{ID: "src", Type: srcType, Params: map[string]any{"channel": "in"}},
			{ID: "snk", Type: snkType, Params: map[string]any{"channel": "out"}},
		},
		Edges: []GraphEdge{{From: "src:out", To: "snk:in"}},
	}
	eng, err := Build(g, DefaultRegistry(), tick)
	if err != nil {
		t.Fatalf("build %s->%s: %v", srcType, snkType, err)
	}
	vd := NewVirtualDriver(
		Channel{Address: "vin", Label: "in", Kind: kind},
		Channel{Address: "vout", Label: "out", Kind: kind},
	)
	reg := NewDriverRegistry()
	reg.RegisterSource(PrefixVirtual, vd)
	reg.RegisterSink(PrefixVirtual, vd)
	table := BindingTable{
		"in":  {Prefix: PrefixVirtual, Addr: "vin"},
		"out": {Prefix: PrefixVirtual, Addr: "vout"},
	}
	if err := BindGraph(eng, g, table, nil, reg); err != nil {
		t.Fatalf("bind %s->%s: %v", srcType, snkType, err)
	}
	return eng, vd
}

// TestAdapterFloatPath proves the whole adapter path now carries Float:
// async source set -> tick queue -> eval -> float sink, with the leading
// zero-of-kind suppressed and the same input sequence reproduced.
func TestAdapterFloatPath(t *testing.T) {
	const tick = 100 * time.Millisecond
	collect := func() []Value {
		eng, vd := buildKindGraph(t, TypeSourceChannelFloat, TypeSinkChannelFloat, Float, tick)
		vd.SetSource("vin", FloatVal(0)) // leading zero-of-kind: not a write
		eng.Tick()
		vd.SetSource("vin", FloatVal(42.5))
		eng.Tick()
		vd.SetSource("vin", FloatVal(-3.25))
		eng.Tick()
		return vd.SinkWrites("vout")
	}
	got := collect()
	want := []float64{42.5, -3.25} // the leading 0 is suppressed
	if len(got) != len(want) {
		t.Fatalf("float sink writes = %+v, want %v (leading zero suppressed)", got, want)
	}
	for i, w := range want {
		if got[i].Kind != Float || got[i].F != w {
			t.Errorf("write[%d] = %+v, want FloatVal(%v)", i, got[i], w)
		}
	}
	if !reflect.DeepEqual(got, collect()) {
		t.Errorf("non-deterministic float path: %+v vs second run", got)
	}
}

// TestAdapterTextPath proves the same path carries Text, leading empty
// string suppressed.
func TestAdapterTextPath(t *testing.T) {
	const tick = 100 * time.Millisecond
	eng, vd := buildKindGraph(t, TypeSourceChannelText, TypeSinkChannelText, Text, tick)
	vd.SetSource("vin", TextVal("")) // leading empty: not a write
	eng.Tick()
	vd.SetSource("vin", TextVal("hello"))
	eng.Tick()
	vd.SetSource("vin", TextVal("world"))
	eng.Tick()
	got := vd.SinkWrites("vout")
	want := []string{"hello", "world"}
	if len(got) != len(want) {
		t.Fatalf("text sink writes = %+v, want %v", got, want)
	}
	for i, w := range want {
		if got[i].Kind != Text || got[i].S != w {
			t.Errorf("write[%d] = %+v, want TextVal(%q)", i, got[i], w)
		}
	}
}

// TestAdapterTypedAsyncLandsNextTick proves a Float Source callback fired
// from a real goroutine only STAGES the typed value (nothing evaluates),
// and one Tick applies it - the same async->queue->tick contract as Bool,
// now carrying a typed Value. Under -race it guards the typed async path
// never touches eval.
func TestAdapterTypedAsyncLandsNextTick(t *testing.T) {
	const tick = 100 * time.Millisecond
	eng, vd := buildKindGraph(t, TypeSourceChannelFloat, TypeSinkChannelFloat, Float, tick)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		vd.SetSource("vin", FloatVal(7.5)) // from another goroutine
	}()
	wg.Wait()

	if got := len(eng.pending); got != 1 {
		t.Fatalf("expected 1 typed value staged in the tick queue, got %d", got)
	}
	if v := eng.nodes["src"].(*sourceChannel).val; v.F != 0 {
		t.Fatalf("async value reached the source node before any Tick (val=%+v); it must defer", v)
	}
	if w := vd.SinkWrites("vout"); len(w) != 0 {
		t.Fatalf("sink wrote before any Tick: %+v", w)
	}

	eng.Tick()

	if got := len(eng.pending); got != 0 {
		t.Errorf("tick queue not drained after one Tick: %d left", got)
	}
	if w := vd.SinkWrites("vout"); len(w) != 1 || w[0].Kind != Float || w[0].F != 7.5 {
		t.Fatalf("after one Tick, float sink writes = %+v; want [FloatVal(7.5)]", w)
	}
}
