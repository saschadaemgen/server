package engine

import (
	"testing"
	"time"
)

// --- test-only building blocks -------------------------------------

// passthru is a non-boundary bool relay (in -> out). It lets tests
// build combinational loops and feedback paths the builtins can't.
type passthru struct{}

func (passthru) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	out.SetBool("out", in.Bool("in"))
}

// fnum is a non-boundary float source, used to provoke kind mismatches.
type fnum struct{}

func (fnum) Eval(ctx *EvalContext, in Inputs, out Outputs) {
	out.SetFloat("out", 0)
}

// testRegistry is the builtin catalog plus the throwaway test blocks.
func testRegistry() *Registry {
	reg := DefaultRegistry().Clone()
	reg.Register(Descriptor{
		Type:    "test.passthru",
		Inputs:  []Port{{Name: "in", Kind: Bool}},
		Outputs: []Port{{Name: "out", Kind: Bool}},
		New:     func(map[string]Value) Node { return passthru{} },
	})
	reg.Register(Descriptor{
		Type:    "test.fnum",
		Outputs: []Port{{Name: "out", Kind: Float}},
		New:     func(map[string]Value) Node { return fnum{} },
	})
	return reg
}

func mustParse(t *testing.T, src string) Graph {
	t.Helper()
	g, err := ParseGraph([]byte(src))
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	return g
}

func hasCode(issues []Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func countCode(issues []Issue, code string) int {
	n := 0
	for _, i := range issues {
		if i.Code == code {
			n++
		}
	}
	return n
}

// --- valid graph: end-to-end JSON -> validate -> build -> run ------

const staircaseJSON = `{
  "schema": 1,
  "nodes": [
    {"id": "btn",   "type": "input.manual"},
    {"id": "stair", "type": "time.staircase", "params": {"duration": 3}, "ui": {"x": 10, "y": 20}},
    {"id": "lamp",  "type": "output.lamp"}
  ],
  "edges": [
    {"from": "btn:out",  "to": "stair:trig"},
    {"from": "stair:q",  "to": "lamp:set"}
  ]
}`

func TestValidateAndBuildStaircase(t *testing.T) {
	g := mustParse(t, staircaseJSON)

	if issues := Validate(g, DefaultRegistry()); hasErrors(issues) {
		t.Fatalf("valid graph reported errors: %+v", issues)
	}

	eng, err := Build(g, DefaultRegistry(), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	base := eng.Now()
	lmp := eng.nodes["lamp"].(*lamp)

	for i := 1; i <= 60; i++ {
		switch time.Duration(i) * 100 * time.Millisecond {
		case 300 * time.Millisecond:
			eng.SetInput("btn", "out", BoolVal(true))
		case 1000 * time.Millisecond:
			eng.SetInput("btn", "out", BoolVal(false))
		case 2000 * time.Millisecond:
			eng.SetInput("btn", "out", BoolVal(true))
		}
		eng.Tick()
	}

	changes := lmp.Changes()
	if len(changes) != 2 {
		t.Fatalf("expected 2 lamp transitions, got %d: %+v", len(changes), changes)
	}
	if got := changes[0].At.Sub(base); got != 300*time.Millisecond || !changes[0].On {
		t.Errorf("transition 0 = (%v, on=%v); want (300ms, true)", got, changes[0].On)
	}
	if got := changes[1].At.Sub(base); got != 5000*time.Millisecond || changes[1].On {
		t.Errorf("transition 1 = (%v, on=%v); want (5000ms, false)", got, changes[1].On)
	}
}

// --- legal self-hold: feedback through a delay boundary ------------

func TestValidateLegalFeedbackThroughBoundary(t *testing.T) {
	// p.out -> s.trig and s.q -> p.in. The loop closes through the
	// staircase, a delay boundary, so the cut breaks it: legal.
	src := `{
      "schema": 1,
      "nodes": [
        {"id": "p", "type": "test.passthru"},
        {"id": "s", "type": "time.staircase", "params": {"duration": 1}}
      ],
      "edges": [
        {"from": "p:out", "to": "s:trig"},
        {"from": "s:q",   "to": "p:in"}
      ]
    }`
	g := mustParse(t, src)
	reg := testRegistry()

	if issues := Validate(g, reg); hasErrors(issues) {
		t.Fatalf("legal feedback reported errors: %+v", issues)
	}
	eng, err := Build(g, reg, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Must run without panicking (the boundary cut makes it orderable).
	for i := 0; i < 20; i++ {
		eng.Tick()
	}
}

// --- combinational cycle: two non-boundary nodes in a ring ---------

func TestValidateCombinationalCycle(t *testing.T) {
	src := `{
      "schema": 1,
      "nodes": [
        {"id": "a", "type": "test.passthru"},
        {"id": "b", "type": "test.passthru"}
      ],
      "edges": [
        {"from": "a:out", "to": "b:in"},
        {"from": "b:out", "to": "a:in"}
      ]
    }`
	g := mustParse(t, src)
	reg := testRegistry()

	issues := Validate(g, reg)
	if n := countCode(issues, CodeCombinationalCycle); n != 2 {
		t.Errorf("expected 2 combinational_cycle issues, got %d: %+v", n, issues)
	}
	if _, err := Build(g, reg, 100*time.Millisecond); err == nil {
		t.Errorf("Build accepted a combinational cycle")
	}
}

// --- one negative test per error class -----------------------------

func TestValidateUnknownType(t *testing.T) {
	g := mustParse(t, `{"schema":1,"nodes":[{"id":"x","type":"does.not.exist"}],"edges":[]}`)
	issues := Validate(g, DefaultRegistry())
	if !hasCode(issues, CodeUnknownType) {
		t.Errorf("expected unknown_type, got %+v", issues)
	}
	if _, err := Build(g, DefaultRegistry(), 100*time.Millisecond); err == nil {
		t.Errorf("Build accepted an unknown type")
	}
}

func TestValidateKindMismatch(t *testing.T) {
	// float output -> bool input.
	g := mustParse(t, `{
      "schema":1,
      "nodes":[
        {"id":"f","type":"test.fnum"},
        {"id":"lamp","type":"output.lamp"}
      ],
      "edges":[{"from":"f:out","to":"lamp:set"}]
    }`)
	issues := Validate(g, testRegistry())
	if !hasCode(issues, CodeKindMismatch) {
		t.Errorf("expected kind_mismatch, got %+v", issues)
	}
}

func TestValidateDoubleDrivenInput(t *testing.T) {
	// Two sources fan into the same input port.
	g := mustParse(t, `{
      "schema":1,
      "nodes":[
        {"id":"b1","type":"input.manual"},
        {"id":"b2","type":"input.manual"},
        {"id":"stair","type":"time.staircase","params":{"duration":1}},
        {"id":"lamp","type":"output.lamp"}
      ],
      "edges":[
        {"from":"b1:out","to":"stair:trig"},
        {"from":"b2:out","to":"stair:trig"},
        {"from":"stair:q","to":"lamp:set"}
      ]
    }`)
	issues := Validate(g, DefaultRegistry())
	if !hasCode(issues, CodeDoubleDrivenInput) {
		t.Errorf("expected double_driven_input, got %+v", issues)
	}
}

func TestValidateMissingRequired(t *testing.T) {
	// stair.trig (required) is left unwired.
	g := mustParse(t, `{
      "schema":1,
      "nodes":[
        {"id":"stair","type":"time.staircase","params":{"duration":1}},
        {"id":"lamp","type":"output.lamp"}
      ],
      "edges":[{"from":"stair:q","to":"lamp:set"}]
    }`)
	issues := Validate(g, DefaultRegistry())
	if !hasCode(issues, CodeMissingRequired) {
		t.Errorf("expected missing_required, got %+v", issues)
	}
}
