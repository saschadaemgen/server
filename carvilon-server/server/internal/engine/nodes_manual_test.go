package engine

import (
	"testing"
	"time"
)

// TestToggleHolds confirms input.toggle is a held source: once set, its
// output stays at that level across ticks (no auto-release), and it can
// be flipped back - the held on/off manual switch behind the "Switch"
// palette block.
func TestToggleHolds(t *testing.T) {
	const tick = 100 * time.Millisecond
	eng := New(tick)
	if _, err := eng.AddType("sw", "input.toggle", nil); err != nil {
		t.Fatalf("add toggle: %v", err)
	}
	lampNode, err := eng.AddType("lamp", "output.lamp", nil)
	if err != nil {
		t.Fatalf("add lamp: %v", err)
	}
	lmp := lampNode.(*lamp)
	eng.Connect("sw", "out", "lamp", "set")

	eng.SetInput("sw", "out", BoolVal(true))
	for i := 0; i < 10; i++ { // ten ticks with no further input
		eng.Tick()
	}
	eng.SetInput("sw", "out", BoolVal(false))
	for i := 0; i < 10; i++ {
		eng.Tick()
	}

	changes := lmp.Changes()
	if len(changes) != 2 {
		t.Fatalf("expected on then off (2 transitions), got %d: %+v", len(changes), changes)
	}
	if !changes[0].On || changes[1].On {
		t.Errorf("transitions = %+v, want [on, off]", changes)
	}
}

// TestConstantEmitsHeldValue confirms each input.constant.<kind> emits
// its "value" param, held, every tick and in the right kind.
func TestConstantEmitsHeldValue(t *testing.T) {
	cases := []struct {
		typ   string
		param Value
		want  Value
	}{
		{"input.constant.bool", BoolVal(true), BoolVal(true)},
		{"input.constant.float", FloatVal(21.5), FloatVal(21.5)},
		{"input.constant.text", TextVal("hi"), TextVal("hi")},
		{"input.constant.bool", Value{}, BoolVal(false)}, // default when omitted
	}
	for _, tc := range cases {
		eng := New(100 * time.Millisecond)
		var params map[string]Value
		if tc.param != (Value{}) {
			params = map[string]Value{"value": tc.param}
		}
		if _, err := eng.AddType("k", tc.typ, params); err != nil {
			t.Fatalf("%s: add: %v", tc.typ, err)
		}
		eng.Tick()
		got, ok := eng.outs["k"]["out"]
		if !ok {
			t.Fatalf("%s: no output on 'out'", tc.typ)
		}
		if got != tc.want {
			t.Errorf("%s: out = %+v, want %+v", tc.typ, got, tc.want)
		}
	}
}
