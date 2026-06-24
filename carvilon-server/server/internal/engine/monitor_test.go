package engine

import (
	"testing"
	"time"
)

// TestMonitorFrames is the ENGINE-S1-02 contract: drive the S1-01
// staircase graph on the manual clock with a monitor subscribed, and
// assert the observed frames reproduce the authoritative trace.
func TestMonitorFrames(t *testing.T) {
	const tick = 100 * time.Millisecond
	eng := New(tick)

	if _, err := eng.AddType("btn", "input.manual", nil); err != nil {
		t.Fatalf("add btn: %v", err)
	}
	if _, err := eng.AddType("stair", "time.staircase", map[string]Value{
		"duration": FloatVal(3),
	}); err != nil {
		t.Fatalf("add staircase: %v", err)
	}
	if _, err := eng.AddType("lamp", "output.lamp", nil); err != nil {
		t.Fatalf("add lamp: %v", err)
	}
	eng.Connect("btn", "out", "stair", "trig")
	eng.Connect("stair", "q", "lamp", "set")

	// Snapshot before the first tick must show the known off-state on
	// the lamp's set wire.
	if c, ok := findChange(eng.Snapshot(), "lamp", "set"); !ok {
		t.Fatalf("snapshot is missing lamp:set")
	} else if c.Value.B {
		t.Errorf("snapshot lamp:set = true; want false (off before first tick)")
	}

	// Buffer larger than the number of frames so nothing is dropped.
	frames, cancel := eng.Subscribe(256)

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

	cancel() // closes the channel; remaining buffered frames still drain
	type lampEvent struct {
		ms int64
		on bool
	}
	var lampEvents []lampEvent
	for f := range frames {
		for _, c := range f.Changes {
			if c.Node == "lamp" && c.Port == "set" {
				lampEvents = append(lampEvents, lampEvent{ms: f.TimeMs, on: c.Value.B})
			}
		}
	}

	if len(lampEvents) != 2 {
		t.Fatalf("expected exactly 2 lamp:set frames, got %d: %+v", len(lampEvents), lampEvents)
	}
	if lampEvents[0] != (lampEvent{ms: 300, on: true}) {
		t.Errorf("lamp:set #0 = %+v; want {ms:300 on:true}", lampEvents[0])
	}
	if lampEvents[1] != (lampEvent{ms: 5000, on: false}) {
		t.Errorf("lamp:set #1 = %+v; want {ms:5000 on:false}", lampEvents[1])
	}
	for _, ev := range lampEvents {
		if ev.ms == 3300 {
			t.Errorf("lamp:set changed at 3300ms; re-trigger must extend to 5000ms")
		}
	}
}

func findChange(cs []Change, node, port string) (Change, bool) {
	for _, c := range cs {
		if c.Node == node && c.Port == port {
			return c, true
		}
	}
	return Change{}, false
}
