// Package mideaengine hosts the Midea climate control loop as a stateful engine
// editor block (E2). The block wraps the climate module's controller (the
// measured strategy) bound to one adopted device: it reads the device's OWN
// return-air sensor and drives the device through the shared monitor connection
// (the Device seam, so package engine stays free of any vendor dependency),
// while its wired inputs (external room temperature, humidity, target, enable,
// lamp feedforward) are the control variables and its outputs are the live
// readouts. Advanced profile only; switching a device to standard deactivates
// the drive (the loop keeps computing readouts but stops writing the device).
package mideaengine

import (
	"math"
	"time"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/mideaclimate"
)

// TypeControlLoop is the engine node type + the fixed port/param names.
const TypeControlLoop = "midea.control_loop"

const (
	ParamDevice    = "device"
	ParamProfile   = "profile"
	ParamTargetHum = "target_hum"

	inRoomTemp = "room_temp"
	inRoomHum  = "room_hum"
	inTarget   = "target"
	inEnable   = "enable"
	inLightOn  = "light_on"
	inLightInS = "light_in_s"

	outStatus    = "status"
	outDeviation = "deviation"
	outTendency  = "tendency"
	outDewpoint  = "dewpoint"
	outVPD       = "vpd"
	outCoolRate  = "cool_rate"
	outAlarm     = "alarm"

	// tickPeriod is how often the loop re-arms itself (the designer run tick).
	tickPeriod = 100 * time.Millisecond
	// defaultTarget matches the module manifest's target default (used when the
	// target input is left unwired).
	defaultTarget = 25.0
)

// Device is the seam the node uses to reach the bound Midea device. Implemented
// by an adapter over mideamonitor.Monitor (+ the store for the profile gate),
// installed via SetDevice. Declared here so package engine never imports the
// vendor monitor (which imports engine).
type Device interface {
	// ClimateStatus returns the device's own return-air sensor and whether the
	// loop may drive it (adopted AND advanced profile). enabled=false → the node
	// keeps computing but does not write the device (gone / switched to standard).
	ClimateStatus(id string) (deviceTemp float64, hasDevice bool, enabled bool)
	// Drive queues a full control decision to the device (non-blocking).
	Drive(id string, out mideaclimate.Outputs)
}

// deviceSvc is the process-wide seam, set once at startup. Nil makes the node
// inert (no fühler, no drive) but still valid.
var deviceSvc Device

// SetDevice installs the device service (main, once the monitor + store exist).
func SetDevice(svc Device) { deviceSvc = svc }

// Register adds the control_loop node type to the engine default registry so the
// designer catalog can Lookup its ports. Call once at startup, before serving.
func Register() {
	engine.Register(engine.Descriptor{
		Type:     TypeControlLoop,
		Category: "climate",
		Title:    "Midea Climate Controller - control loop",
		Inputs: []engine.Port{
			{Name: inRoomTemp, Kind: engine.Float}, // external sensor (the control variable)
			{Name: inRoomHum, Kind: engine.Float, Optional: true},
			{Name: inTarget, Kind: engine.Float, Optional: true},
			{Name: inEnable, Kind: engine.Bool, Optional: true},
			{Name: inLightOn, Kind: engine.Bool, Optional: true},
			{Name: inLightInS, Kind: engine.Float, Optional: true},
		},
		Outputs: []engine.Port{
			{Name: outStatus, Kind: engine.Text},
			{Name: outDeviation, Kind: engine.Float},
			{Name: outTendency, Kind: engine.Float},
			{Name: outDewpoint, Kind: engine.Float},
			{Name: outVPD, Kind: engine.Float},
			{Name: outCoolRate, Kind: engine.Float},
			{Name: outAlarm, Kind: engine.Text},
		},
		Params: []engine.Param{
			{Name: ParamDevice, Kind: engine.Text, Required: true},
			{Name: ParamProfile, Kind: engine.Text, Default: engine.TextVal(string(mideaclimate.ProfKomfort))},
			{Name: ParamTargetHum, Kind: engine.Float, Default: engine.FloatVal(0)},
		},
		DelayBoundary: true, // stateful loop: its output is served from stored state
		New:           newControlLoop,
	})
}

type controlLoop struct {
	dev         string
	ctrl        *mideaclimate.Controller
	lastNow     time.Time
	prevEnabled bool
}

func newControlLoop(p map[string]engine.Value) engine.Node {
	params := mideaclimate.DefaultParams()
	if v, ok := p[ParamProfile]; ok && v.S != "" {
		params.Profile = mideaclimate.Profile(v.S)
		params.Heizen = params.Profile == mideaclimate.ProfHeizen
		params.LampFeedfwd = params.Profile == mideaclimate.ProfKultivierung
	}
	if v, ok := p[ParamTargetHum]; ok && v.F > 0 {
		params.VpdZiel = v.F
	}
	return &controlLoop{dev: p[ParamDevice].S, ctrl: mideaclimate.New(params)}
}

// selfStart makes the engine evaluate the loop on the first tick even with no
// changing input; each Eval re-arms via WakeAfter to keep it ticking.
func (n *controlLoop) selfStart() {}

func (n *controlLoop) Eval(ctx *engine.EvalContext, in engine.Inputs, out engine.Outputs) {
	defer ctx.WakeAfter(tickPeriod) // self-clock: keep ticking every period

	now := ctx.Now()
	dtMin := tickPeriod.Minutes()
	if !n.lastNow.IsZero() {
		if d := now.Sub(n.lastNow).Minutes(); d > 0 {
			dtMin = d
		}
	}
	n.lastNow = now

	// The bound device's own return-air sensor + the advanced-profile drive gate.
	var deviceTemp float64
	var hasDevice, enabled bool
	if deviceSvc != nil {
		deviceTemp, hasDevice, enabled = deviceSvc.ClimateStatus(n.dev)
	}

	// On the disabled->enabled edge (device switched to advanced, or the status
	// cache populated on the first ticks) reset the controller so it re-issues a
	// fresh command. Otherwise a decision computed while the drive was suppressed
	// would be committed inside Tick (send-once) but never sent, stranding the
	// device (adopted + advanced + demand, yet never commanded, cockpit locked).
	if enabled && !n.prevEnabled {
		n.ctrl.Reset()
	}
	n.prevEnabled = enabled

	target := defaultTarget
	if in.Connected(inTarget) {
		target = in.Float(inTarget)
	}

	ci := mideaclimate.Inputs{
		Now:         now,
		DtMin:       dtMin,
		RoomTemp:    in.Float(inRoomTemp),
		SensorValid: in.Connected(inRoomTemp),
		RoomHum:     in.Float(inRoomHum),
		HasHum:      in.Connected(inRoomHum),
		Target:      target,
		Enable:      !in.Connected(inEnable) || in.Bool(inEnable), // default enabled
		DeviceTemp:  deviceTemp,
		HasDevice:   hasDevice,
		LightOn:     in.Bool(inLightOn),
		LightInS:    in.Float(inLightInS),
		HasLightFF:  in.Connected(inLightOn),
	}

	d, rd := n.ctrl.Tick(ci)

	// Drive the device on a real change, ONLY while the loop is enabled (adopted
	// + advanced profile). Non-blocking: queued to the monitor's apply worker.
	if enabled && d.Send && deviceSvc != nil {
		deviceSvc.Drive(n.dev, d)
	}

	// Readouts always → live faceplate.
	out.SetText(outStatus, rd.Gang)
	out.SetFloat(outDeviation, rd.Abweichung)
	out.SetFloat(outTendency, rd.Tendenz)
	out.SetFloat(outDewpoint, safeFloat(rd.Taupunkt))
	out.SetFloat(outVPD, rd.VPD)
	out.SetFloat(outCoolRate, rd.CoolRate)
	out.SetText(outAlarm, rd.Alarm)
}

// safeFloat scrubs the ±Inf/NaN the dewpoint can produce at 0% humidity so the
// engine's comparable Value stays well-defined.
func safeFloat(f float64) float64 {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return 0
	}
	return f
}
