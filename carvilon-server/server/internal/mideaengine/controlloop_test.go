package mideaengine

import (
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/mideaclimate"
)

// ensureRegistered registers the node once per test binary. The registry panics
// on a duplicate type (main registers exactly once), so every test that needs
// the type goes through here rather than calling Register directly.
var registerOnce sync.Once

func ensureRegistered() { registerOnce.Do(Register) }

// TestRegisterAndFactory checks the control_loop registers with the expected
// ports/params and that the factory maps the profile + target-VPD params onto
// the controller.
func TestRegisterAndFactory(t *testing.T) {
	ensureRegistered()
	d, ok := engine.Lookup(TypeControlLoop)
	if !ok {
		t.Fatal("control_loop not registered in the default registry")
	}
	if len(d.Inputs) != 6 {
		t.Errorf("inputs = %d, want 6", len(d.Inputs))
	}
	if len(d.Outputs) != 7 {
		t.Errorf("outputs = %d, want 7", len(d.Outputs))
	}
	// The device param must be required (no usable default).
	var deviceReq bool
	for _, p := range d.Params {
		if p.Name == ParamDevice {
			deviceReq = p.Required
		}
	}
	if !deviceReq {
		t.Error("device param should be Required")
	}
	if !d.DelayBoundary {
		t.Error("control_loop should be a DelayBoundary (stateful loop)")
	}

	// Factory: profile heizen -> Heizen, cultivation -> LampFeedfwd, target_hum -> VpdZiel.
	n := newControlLoop(map[string]engine.Value{
		ParamDevice:    engine.TextVal("abc"),
		ParamProfile:   engine.TextVal(string(mideaclimate.ProfHeizen)),
		ParamTargetHum: engine.FloatVal(1.2),
	}).(*controlLoop)
	if n.dev != "abc" {
		t.Errorf("dev = %q, want abc", n.dev)
	}
	if !n.ctrl.P.Heizen {
		t.Error("heizen profile not applied to the controller")
	}
	if n.ctrl.P.VpdZiel != 1.2 {
		t.Errorf("VpdZiel = %v, want 1.2", n.ctrl.P.VpdZiel)
	}

	cult := newControlLoop(map[string]engine.Value{
		ParamDevice:  engine.TextVal("x"),
		ParamProfile: engine.TextVal(string(mideaclimate.ProfKultivierung)),
	}).(*controlLoop)
	if !cult.ctrl.P.LampFeedfwd {
		t.Error("cultivation profile should enable lamp feedforward")
	}
	if _, ok := interface{}(cult).(engine.SelfStarter); !ok {
		t.Error("control_loop must be a selfStarter so it runs on tick 1")
	}
}

// fakeDevice records Drive calls to verify the seam wiring.
type fakeDevice struct {
	temp    float64
	has     bool
	enabled bool
	driven  []mideaclimate.Outputs
}

func (f *fakeDevice) ClimateStatus(string) (float64, bool, bool) { return f.temp, f.has, f.enabled }
func (f *fakeDevice) Drive(_ string, out mideaclimate.Outputs)   { f.driven = append(f.driven, out) }

func TestSetDeviceSeam(t *testing.T) {
	fd := &fakeDevice{temp: 26, has: true, enabled: true}
	SetDevice(fd)
	defer SetDevice(nil)
	if deviceSvc == nil {
		t.Fatal("SetDevice did not install the seam")
	}
	temp, has, enabled := deviceSvc.ClimateStatus("abc")
	if temp != 26 || !has || !enabled {
		t.Errorf("ClimateStatus = (%v,%v,%v)", temp, has, enabled)
	}
}

// deviationAfterTick runs one tick of a loop wired to a fixed room temperature
// and returns the deviation readout. Deviation is smoothed room temp minus the
// effective target, so with a single sample it reads back exactly which target
// the loop used — the probe for every manual/wired precedence case below.
func deviationAfterTick(t *testing.T, roomTemp float64, params map[string]engine.Value, wire func(e *engine.Engine)) float64 {
	t.Helper()
	ensureRegistered()
	e := engine.New(100 * time.Millisecond)
	if _, err := e.AddType("loop", TypeControlLoop, params); err != nil {
		t.Fatalf("AddType: %v", err)
	}
	if _, err := e.AddType("room", "input.constant.float", map[string]engine.Value{"value": engine.FloatVal(roomTemp)}); err != nil {
		t.Fatalf("AddType room: %v", err)
	}
	e.Connect("room", "out", "loop", inRoomTemp)
	if wire != nil {
		wire(e)
	}
	e.Tick()
	for _, c := range e.Snapshot() {
		if c.Node == "loop" && c.Port == outDeviation {
			return c.Value.F
		}
	}
	t.Fatal("no deviation readout in snapshot")
	return 0
}

func baseParams() map[string]engine.Value {
	return map[string]engine.Value{ParamDevice: engine.TextVal("abc")}
}

// TestManualTargetAndEnable covers the block's own controls: the target field
// and enable switch back the value when the matching port is unwired.
func TestManualTargetAndEnable(t *testing.T) {
	SetDevice(&fakeDevice{temp: 26, has: true, enabled: true})
	defer SetDevice(nil)

	// The block's target param is what the loop regulates to: 26 - 20 = 6.
	p := baseParams()
	p[ParamTargetTemp] = engine.FloatVal(20)
	p[ParamEnabled] = engine.BoolVal(true)
	if got := deviationAfterTick(t, 26, p, nil); got != 6 {
		t.Errorf("deviation with manual target 20 = %v, want 6 (target ignored)", got)
	}

	// A target outside the sane range is refused; the loop keeps its default.
	p = baseParams()
	p[ParamTargetTemp] = engine.FloatVal(500)
	if got := deviationAfterTick(t, 26, p, nil); got != 1 {
		t.Errorf("deviation with out-of-range target = %v, want 1 (fell back to %v)", got, defaultTarget)
	}

	// The enable switch off means "stop conditioning this room": the loop sends
	// exactly one OFF command rather than abandoning the unit at its last
	// setpoint. A hot room (30 vs target 25) would otherwise be cooled.
	fd := &fakeDevice{temp: 26, has: true, enabled: true}
	SetDevice(fd)
	p = baseParams()
	p[ParamEnabled] = engine.BoolVal(false)
	deviationAfterTick(t, 30, p, nil)
	if len(fd.driven) != 1 {
		t.Fatalf("enable=false drove the device %d times, want 1 (the OFF command)", len(fd.driven))
	}
	if fd.driven[0].Mode != mideaclimate.ModeOff {
		t.Errorf("enable=false sent mode %v, want %v", fd.driven[0].Mode, mideaclimate.ModeOff)
	}
}

// TestLegacyGraphDefaults pins the compatibility contract: a graph saved before
// the block grew its own controls carries neither param, and must behave exactly
// as it did — 25 °C and enabled.
func TestLegacyGraphDefaults(t *testing.T) {
	fd := &fakeDevice{temp: 26, has: true, enabled: true}
	SetDevice(fd)
	defer SetDevice(nil)
	// 26 - 25 = 1 → the old default target is still in force.
	if got := deviationAfterTick(t, 26, baseParams(), nil); got != 1 {
		t.Errorf("legacy deviation = %v, want 1 (default target %v)", got, defaultTarget)
	}
	// A hot room with no enable param must still drive: default enabled.
	if len(fd.driven) == 0 {
		t.Error("legacy graph did not drive the device — default enable regressed")
	}
}

// TestSetExternalLive covers the live path the faceplate uses: a target typed
// while the run is up reaches the node without a rebuild.
//
// It drives engine.SetInput — the REAL path the HTTP handler takes — and never
// a locally declared interface. That distinction is the whole point: the
// engine's in-package contract uses an UNEXPORTED method, which Go qualifies by
// declaring package, so a mideaengine method of the same name silently fails to
// satisfy it. A test asserting against its own copy of the interface would pass
// while the live path panicked in production. This one panics if the contract
// breaks.
func TestSetExternalLive(t *testing.T) {
	SetDevice(&fakeDevice{temp: 26, has: true, enabled: true})
	defer SetDevice(nil)
	ensureRegistered()

	e := engine.New(100 * time.Millisecond)
	n, err := e.AddType("loop", TypeControlLoop, baseParams())
	if err != nil {
		t.Fatalf("AddType: %v", err)
	}
	cl := n.(*controlLoop)

	// SetInput panics for a node the engine does not recognise as an external
	// setter; the handler's recover turns that into an HTTP 400 that the editor
	// silently swallows, so assert it does NOT panic here.
	e.SetInput("loop", inTarget, engine.FloatVal(21))
	if cl.manTarget != 21 {
		t.Errorf("manTarget after a live target set = %v, want 21", cl.manTarget)
	}
	// Out-of-range values are refused server-side, so a stale or hand-made
	// request cannot command a nonsense setpoint.
	e.SetInput("loop", inTarget, engine.FloatVal(999))
	if cl.manTarget != 21 {
		t.Errorf("manTarget after an out-of-range set = %v, want 21 (unchanged)", cl.manTarget)
	}
	e.SetInput("loop", inEnable, engine.BoolVal(false))
	if cl.manEnable {
		t.Error("a live enable=false did not reach the node")
	}
	// An unknown port is ignored, not a panic: the engine lets the node decide
	// what its ports mean.
	e.SetInput("loop", "nonsense", engine.BoolVal(true))
}

// TestEngineContracts pins the two marker contracts against the ENGINE's own
// exported interfaces. Asserting a locally declared look-alike would be
// meaningless — see TestSetExternalLive.
func TestEngineContracts(t *testing.T) {
	n := newControlLoop(baseParams())
	if _, ok := n.(engine.ExternalSetter); !ok {
		t.Error("control_loop must implement engine.ExternalSetter, or the whole live manual path is dead")
	}
	if _, ok := n.(engine.SelfStarter); !ok {
		t.Error("control_loop must implement engine.SelfStarter, or it never starts ticking on its own")
	}
}

// TestWiredPortsBeatManual is the override rule: a wired target/enable owns the
// value, so the block's own control cannot fight the graph.
func TestWiredPortsBeatManual(t *testing.T) {
	SetDevice(&fakeDevice{temp: 26, has: true, enabled: true})
	defer SetDevice(nil)

	// Manual target says 20, the wire says 22 → the wire wins (26-22 = 4).
	p := baseParams()
	p[ParamTargetTemp] = engine.FloatVal(20)
	got := deviationAfterTick(t, 26, p, func(e *engine.Engine) {
		if _, err := e.AddType("tgt", "input.constant.float", map[string]engine.Value{"value": engine.FloatVal(22)}); err != nil {
			t.Fatalf("AddType tgt: %v", err)
		}
		e.Connect("tgt", "out", "loop", inTarget)
	})
	if got != 4 {
		t.Errorf("deviation with manual 20 + wired 22 = %v, want 4 (the wire must win)", got)
	}

	// The block's switch says ON, the wire says false → the wire wins. A hot
	// room (30 vs the 25 default) would be cooled if the manual switch had won,
	// so the OFF command is what proves the precedence.
	fd := &fakeDevice{temp: 26, has: true, enabled: true}
	SetDevice(fd)
	p = baseParams()
	p[ParamEnabled] = engine.BoolVal(true)
	deviationAfterTick(t, 30, p, func(e *engine.Engine) {
		if _, err := e.AddType("en", "input.constant.bool", map[string]engine.Value{"value": engine.BoolVal(false)}); err != nil {
			t.Fatalf("AddType en: %v", err)
		}
		e.Connect("en", "out", "loop", inEnable)
	})
	if len(fd.driven) != 1 || fd.driven[0].Mode != mideaclimate.ModeOff {
		t.Errorf("wired enable=false gave %+v, want a single OFF command (the wire must beat the block's switch)", fd.driven)
	}
}
