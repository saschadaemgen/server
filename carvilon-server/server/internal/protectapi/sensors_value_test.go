package protectapi

import (
	"math"
	"testing"
	"time"
)

// Float is the numeric side of flexVal used by engine readout ports: it
// parses bare numbers and numeric strings, and reports ok=false for
// absent/object/array/non-numeric values (so a real 0 is distinguishable
// from "no reading").
func TestFlexVal_Float(t *testing.T) {
	cases := []struct {
		name   string
		in     any
		want   float64
		wantOK bool
	}{
		{"float", 21.5, 21.5, true},
		{"int", 48, 48, true},
		{"zero", 0, 0, true},
		{"negative", -62, -62, true},
		{"numeric string", "120", 120, true},
		{"non-numeric string", "good", 0, false},
		{"object", map[string]any{"value": 21.5}, 0, false},
		{"array", []int{87}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := flexJSON(t, c.in).Float()
			if ok != c.wantOK || (ok && math.Abs(got-c.want) > 1e-9) {
				t.Errorf("Float() = (%v, %v), want (%v, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
	// The zero flexVal (absent) is not a number.
	if _, ok := (flexVal{}).Float(); ok {
		t.Errorf("absent flexVal parsed as a number")
	}
}

// The numeric/bool accessors read the raw reading (no unit) with an
// ok flag, mirroring the label accessors' fields.
func TestSensor_ValueAccessors(t *testing.T) {
	s := Sensor{
		Stats: SensorStats{
			Temperature: SensorStat{Value: flexJSON(t, 21.5)},
			Humidity:    SensorStat{Value: flexJSON(t, 48)},
			Light:       SensorStat{Value: flexJSON(t, 120)},
		},
		Battery:          BatteryStatus{Percentage: flexJSON(t, 87)},
		IsMotionDetected: flexJSON(t, true),
		IsOpened:         flexJSON(t, false),
	}

	assertFloat(t, "temperature", s.TemperatureValue, 21.5, true)
	assertFloat(t, "humidity", s.HumidityValue, 48, true)
	assertFloat(t, "light", s.LightValue, 120, true)
	assertFloat(t, "battery", s.BatteryValue, 87, true)

	if v, ok := s.MotionActive(); !ok || !v {
		t.Errorf("MotionActive = (%v,%v), want (true,true)", v, ok)
	}
	if v, ok := s.OpenedActive(); !ok || v {
		t.Errorf("OpenedActive = (%v,%v), want (false,true)", v, ok)
	}

	// A sensor with no measurements reports ok=false everywhere, so a
	// bound Float port holds its last value instead of snapping to 0.
	var empty Sensor
	assertFloat(t, "empty temperature", empty.TemperatureValue, 0, false)
	if _, ok := empty.MotionActive(); ok {
		t.Errorf("absent motion reported ok=true")
	}
}

// Leak/tamper booleans reuse the same freshness window as the labels:
// a fresh timestamp is active, a stale one is present-but-inactive, an
// absent field reports present=false.
func TestSensor_EventActive(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	fresh := flexJSON(t, now.Add(-recentEventWindow/2).UnixMilli())
	stale := flexJSON(t, now.Add(-2*recentEventWindow).UnixMilli())

	s := Sensor{LeakDetectedAt: fresh, TamperingDetectedAt: stale}
	if active, present := s.LeakActive(now); !present || !active {
		t.Errorf("fresh leak = (%v,%v), want (true,true)", active, present)
	}
	if active, present := s.TamperActive(now); !present || active {
		t.Errorf("stale tamper = (%v,%v), want active=false present=true", active, present)
	}
	var empty Sensor
	if active, present := empty.LeakActive(now); present || active {
		t.Errorf("absent leak = (%v,%v), want (false,false)", active, present)
	}
}

func assertFloat(t *testing.T, name string, fn func() (float64, bool), want float64, wantOK bool) {
	t.Helper()
	got, ok := fn()
	if ok != wantOK || (ok && math.Abs(got-want) > 1e-9) {
		t.Errorf("%s = (%v, %v), want (%v, %v)", name, got, ok, want, wantOK)
	}
}
