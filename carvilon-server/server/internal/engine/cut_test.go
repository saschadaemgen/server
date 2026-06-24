package engine

import (
	"testing"
	"time"
)

// The graph under test (ENGINE-S1-06b). The staircase is the only
// delay boundary; its output fans out two ways:
//
//	src.out -> or.a
//	or.out  -> stair.trig
//	stair.q -> or.b      (FEEDBACK    - closes the or<->stair cycle)
//	stair.q -> lamp.set  (FORWARD     - no cycle, must stay same tick)
//
// The cut must sever only the cycle-closing stair->or.b and keep the
// forward stair->lamp.set, so lamp is ordered after stair and turns
// on in the same tick the staircase fires (300ms, not 400ms).

func mustType(t *testing.T, e *Engine, id, typ string, params map[string]Value) Node {
	t.Helper()
	n, err := e.AddType(id, typ, params)
	if err != nil {
		t.Fatalf("AddType %s (%s): %v", id, typ, err)
	}
	return n
}

// orFeedbackEngine builds the graph above. connectFwdFirst flips the
// order in which the two stair.q fan-out edges are wired, to prove the
// cut choice is independent of insertion order.
func orFeedbackEngine(t *testing.T, reverseNodes, connectFwdFirst bool) (*Engine, *lamp) {
	t.Helper()
	e := New(100 * time.Millisecond)
	add := func() {
		mustType(t, e, "src", "input.manual", nil)
		mustType(t, e, "or", "logic.or", nil)
		mustType(t, e, "stair", "time.staircase", map[string]Value{"duration": FloatVal(3)})
		mustType(t, e, "lamp", "output.lamp", nil)
	}
	addReversed := func() {
		mustType(t, e, "lamp", "output.lamp", nil)
		mustType(t, e, "stair", "time.staircase", map[string]Value{"duration": FloatVal(3)})
		mustType(t, e, "or", "logic.or", nil)
		mustType(t, e, "src", "input.manual", nil)
	}
	if reverseNodes {
		addReversed()
	} else {
		add()
	}

	e.Connect("src", "out", "or", "a")
	e.Connect("or", "out", "stair", "trig")
	if connectFwdFirst {
		e.Connect("stair", "q", "lamp", "set") // forward first
		e.Connect("stair", "q", "or", "b")     // feedback second
	} else {
		e.Connect("stair", "q", "or", "b")     // feedback first
		e.Connect("stair", "q", "lamp", "set") // forward second
	}
	return e, e.nodes["lamp"].(*lamp)
}

// orFeedbackGraph is the declarative form, for the validation check.
func orFeedbackGraph() Graph {
	return Graph{
		Schema: 1,
		Nodes: []GraphNode{
			{ID: "src", Type: "input.manual"},
			{ID: "or", Type: "logic.or"},
			{ID: "stair", Type: "time.staircase", Params: map[string]any{"duration": 3.0}},
			{ID: "lamp", Type: "output.lamp"},
		},
		Edges: []GraphEdge{
			{From: "src:out", To: "or:a"},
			{From: "or:out", To: "stair:trig"},
			{From: "stair:q", To: "or:b"},
			{From: "stair:q", To: "lamp:set"},
		},
	}
}

// TestForwardEdgeStaysSameTick is the headline guard: the forward
// boundary edge survives the cut, so the lamp turns on at 300ms (the
// tick the staircase fires) and not one tick later at 400ms.
func TestForwardEdgeStaysSameTick(t *testing.T) {
	e, lmp := orFeedbackEngine(t, false, false)
	base := e.Now()

	for i := 1; i <= 40; i++ {
		if time.Duration(i)*100*time.Millisecond == 300*time.Millisecond {
			e.SetInput("src", "out", BoolVal(true))
		}
		e.Tick() // a wrong (eager) cut would panic or misorder; neither is allowed
	}

	changes := lmp.Changes()
	if len(changes) == 0 {
		t.Fatalf("lamp never turned on")
	}
	if got := changes[0].At.Sub(base); got != 300*time.Millisecond || !changes[0].On {
		t.Fatalf("lamp first transition = (%v, on=%v); want (300ms, true) - the forward edge "+
			"stair->lamp must stay same-tick, not be delayed to 400ms", got, changes[0].On)
	}

	if issues := Validate(orFeedbackGraph(), DefaultRegistry()); hasErrors(issues) {
		t.Errorf("graph should validate cleanly, got: %+v", issues)
	}
}

// TestCutDeterminism builds the same logical graph with nodes and
// edges inserted in different orders and requires an identical eval
// order and identical lamp trace - the cut choice must be reproducible.
func TestCutDeterminism(t *testing.T) {
	run := func(reverseNodes, fwdFirst bool) (order []string, changes []LampChange) {
		e, lmp := orFeedbackEngine(t, reverseNodes, fwdFirst)
		for i := 1; i <= 60; i++ {
			if time.Duration(i)*100*time.Millisecond == 300*time.Millisecond {
				e.SetInput("src", "out", BoolVal(true))
			}
			e.Tick()
		}
		return e.topo(), lmp.Changes()
	}

	order0, trace0 := run(false, false)
	order1, trace1 := run(true, true)

	if want := []string{"src", "or", "stair", "lamp"}; !equalStrings(order0, want) {
		t.Errorf("eval order = %v; want %v", order0, want)
	}
	if !equalStrings(order0, order1) {
		t.Errorf("eval order not deterministic: %v vs %v", order0, order1)
	}
	if !equalLampChanges(trace0, trace1) {
		t.Errorf("trace not deterministic: %v vs %v", trace0, trace1)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalLampChanges(a, b []LampChange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
