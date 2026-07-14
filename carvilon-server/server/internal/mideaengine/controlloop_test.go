package mideaengine

import (
	"testing"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/mideaclimate"
)

// TestRegisterAndFactory checks the control_loop registers with the expected
// ports/params and that the factory maps the profile + target-VPD params onto
// the controller.
func TestRegisterAndFactory(t *testing.T) {
	Register()
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
	if _, ok := interface{}(cult).(interface{ selfStart() }); !ok {
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
