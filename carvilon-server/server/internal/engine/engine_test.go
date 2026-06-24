package engine

import (
	"testing"
	"time"
)

// TestStaircaseTrace is the ENGINE-S1-01 definition of done. It runs
// the 3-node graph
//
//	btn (input.manual) -> staircase (time.staircase, 3s) -> lamp
//
// on a 100ms logical tick and asserts the authoritative trace:
//
//	button pressed at 300ms   -> lamp ON  at 300ms
//	re-trigger     at 2000ms  -> stays ON
//	                          -> lamp OFF at 5000ms  (2000+3000, NOT 3300)
func TestStaircaseTrace(t *testing.T) {
	const tick = 100 * time.Millisecond
	eng := New(tick)
	base := eng.Now() // logical start

	if _, err := eng.AddType("btn", "input.manual", nil); err != nil {
		t.Fatalf("add btn: %v", err)
	}
	if _, err := eng.AddType("stair", "time.staircase", map[string]Value{
		"duration": FloatVal(3), // seconds
	}); err != nil {
		t.Fatalf("add staircase: %v", err)
	}
	lampNode, err := eng.AddType("lamp", "output.lamp", nil)
	if err != nil {
		t.Fatalf("add lamp: %v", err)
	}
	lmp := lampNode.(*lamp)

	eng.Connect("btn", "out", "stair", "trig")
	eng.Connect("stair", "q", "lamp", "set")

	// Drive 6s of logical time at 100ms per tick. Inject just before
	// the tick that should observe the input. The release at 1000ms
	// gives the 2000ms press a clean rising edge to re-trigger on.
	for i := 1; i <= 60; i++ {
		switch time.Duration(i) * tick {
		case 300 * time.Millisecond:
			eng.SetInput("btn", "out", BoolVal(true))
		case 1000 * time.Millisecond:
			eng.SetInput("btn", "out", BoolVal(false))
		case 2000 * time.Millisecond:
			eng.SetInput("btn", "out", BoolVal(true)) // re-trigger
		}
		eng.Tick()
	}

	changes := lmp.Changes()
	if len(changes) != 2 {
		t.Fatalf("expected exactly 2 lamp transitions, got %d: %+v", len(changes), offsets(base, changes))
	}

	if got := changes[0].At.Sub(base); got != 300*time.Millisecond || !changes[0].On {
		t.Errorf("transition 0 = (%v, on=%v); want (300ms, on=true)", got, changes[0].On)
	}
	if got := changes[1].At.Sub(base); got != 5000*time.Millisecond || changes[1].On {
		t.Errorf("transition 1 = (%v, on=%v); want (5000ms, on=false)", got, changes[1].On)
	}

	// Guard the headline invariant explicitly: the lamp must NOT have
	// switched at 3300ms (the first trigger's would-be expiry).
	for _, c := range changes {
		if c.At.Sub(base) == 3300*time.Millisecond {
			t.Errorf("lamp switched at 3300ms; re-trigger must extend to 5000ms")
		}
	}

	// The Emit stub must mirror the same trace.
	evs := eng.Events()
	if len(evs) != 2 {
		t.Fatalf("expected 2 emitted events, got %d", len(evs))
	}
	if evs[0].At.Sub(base) != 300*time.Millisecond || !evs[0].Value.B {
		t.Errorf("event 0 = (%v, %v); want (300ms, true)", evs[0].At.Sub(base), evs[0].Value.B)
	}
	if evs[1].At.Sub(base) != 5000*time.Millisecond || evs[1].Value.B {
		t.Errorf("event 1 = (%v, %v); want (5000ms, false)", evs[1].At.Sub(base), evs[1].Value.B)
	}
}

// TestTopoCycleDetected confirms the kernel refuses a cyclic graph
// (the validator's job is upstream, but the kernel guards its own
// precondition rather than looping forever).
func TestTopoCycleDetected(t *testing.T) {
	eng := New(100 * time.Millisecond)
	eng.Add("a", &manual{})
	eng.Add("b", &lamp{})
	// a.out -> b.set and b (as if it had an out) -> a.* would be a
	// cycle; emulate with a back-edge that creates one.
	eng.Connect("a", "out", "b", "set")
	eng.Connect("b", "out", "a", "in") // back-edge: a depends on b, b on a

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on cyclic graph")
		}
	}()
	eng.SetInput("a", "out", BoolVal(true))
	eng.Tick() // triggers topo(), which must panic on the cycle
}

func offsets(base time.Time, cs []LampChange) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		state := "off"
		if c.On {
			state = "on"
		}
		out[i] = c.At.Sub(base).String() + ":" + state
	}
	return out
}
