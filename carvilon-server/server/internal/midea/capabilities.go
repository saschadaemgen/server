package midea

import (
	"encoding/binary"
	"errors"
)

const responseCapabilities = 0xB5

// Capability-IDs des 0xB5-Response (16-Bit, little-endian im Frame).
const (
	capSelfClean   = 0x0039
	capFanControl  = 0x0210
	capPresetEco   = 0x0212
	capFreeze      = 0x0213
	capModes       = 0x0214
	capSwingModes  = 0x0215
	capAnion       = 0x021E
	capPresetTurbo = 0x021A
	capFahrenheit  = 0x0222
	capTemperature = 0x0225
	capBuzzer      = 0x022C
)

// Capabilities beschreibt, was ein konkretes Gerät kann (aus 0xB5 gelesen).
// Die Has*-Flags sagen, ob der jeweilige Fähigkeits-Eintrag überhaupt gemeldet
// wurde – wichtig, um zwischen "nicht unterstützt" und "nicht gemeldet" zu
// unterscheiden.
type Capabilities struct {
	HasModes                              bool
	CoolMode, HeatMode, DryMode, AutoMode bool

	HasFanControl                                  bool
	FanSilent, FanLow, FanMedium, FanHigh, FanAuto bool

	HasSwingModes                  bool
	SwingHorizontal, SwingVertical bool

	Eco              bool
	FreezeProtection bool
	TurboCool        bool
	TurboHeat        bool
	Fahrenheit       bool
	Buzzer           bool
	Anion            bool
	SelfClean        bool

	HasTemperatures                                      bool
	CoolMin, CoolMax, AutoMin, AutoMax, HeatMin, HeatMax float64

	Additional bool // Gerät meldet weitere Fähigkeiten (zweiter 0xB5-Aufruf)
}

// ErrNotCapabilities signalisiert, dass ein Frame kein 0xB5-Response ist.
var ErrNotCapabilities = errors.New("midea: Frame ist kein Fähigkeits-Response")

func anyOf(v byte, set ...byte) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// ParseCapabilities liest einen 0xB5-Fähigkeits-Response.
func ParseCapabilities(frame []byte) (*Capabilities, error) {
	body, err := frameBody(frame)
	if err != nil {
		return nil, err
	}
	if len(body) < 2 || body[0] != responseCapabilities {
		return nil, ErrNotCapabilities
	}

	count := int(body[1])
	caps := body[2:]
	c := &Capabilities{}

	for i := 0; i < count; i++ {
		if len(caps) < 3 {
			break
		}
		size := int(caps[2])
		if size == 0 {
			caps = caps[3:]
			continue
		}
		if 3+size > len(caps) {
			break
		}
		id := binary.LittleEndian.Uint16(caps[0:2])
		val := caps[3 : 3+size]
		v := val[0]

		switch id {
		case capModes:
			c.HasModes = true
			c.HeatMode = anyOf(v, 1, 2, 4, 6, 7, 9, 10, 11, 12, 13)
			c.CoolMode = anyOf(v, 0, 1, 3, 4, 5, 6, 7, 8, 9, 11, 13, 14, 15)
			c.DryMode = anyOf(v, 0, 1, 5, 6, 9, 11, 13, 14, 15)
			c.AutoMode = anyOf(v, 0, 1, 2, 7, 8, 9, 13, 14)
		case capFanControl:
			c.HasFanControl = true
			c.FanSilent = anyOf(v, 6, 9)
			c.FanLow = anyOf(v, 3, 4, 5, 6, 7, 9)
			c.FanMedium = anyOf(v, 5, 6, 7)
			c.FanHigh = anyOf(v, 3, 4, 5, 6, 7, 9)
			c.FanAuto = anyOf(v, 4, 5, 6, 9)
		case capSwingModes:
			c.HasSwingModes = true
			c.SwingHorizontal = anyOf(v, 1, 3)
			c.SwingVertical = anyOf(v, 0, 1)
		case capPresetEco:
			c.Eco = anyOf(v, 1, 2)
		case capFreeze:
			c.FreezeProtection = v == 1
		case capPresetTurbo:
			c.TurboHeat = anyOf(v, 1, 3)
			c.TurboCool = anyOf(v, 0, 1)
		case capFahrenheit:
			c.Fahrenheit = v == 0
		case capBuzzer:
			c.Buzzer = v == 1
		case capAnion:
			c.Anion = v == 1
		case capSelfClean:
			c.SelfClean = v == 1
		case capTemperature:
			if size >= 6 {
				c.HasTemperatures = true
				c.CoolMin = float64(val[0]) * 0.5
				c.CoolMax = float64(val[1]) * 0.5
				c.AutoMin = float64(val[2]) * 0.5
				c.AutoMax = float64(val[3]) * 0.5
				c.HeatMin = float64(val[4]) * 0.5
				c.HeatMax = float64(val[5]) * 0.5
			}
		}
		caps = caps[3+size:]
	}

	if len(caps) > 1 {
		c.Additional = caps[len(caps)-2] != 0
	}
	return c, nil
}

// TargetRange liefert den gemeinsamen Zieltemperaturbereich über alle Modi.
// Fällt auf 16–30 °C zurück, wenn keine Temperatur-Fähigkeit gemeldet wurde.
func (c *Capabilities) TargetRange() (min, max float64) {
	if !c.HasTemperatures {
		return 16, 30
	}
	min, max = 100, 0
	for _, p := range [][2]float64{{c.CoolMin, c.CoolMax}, {c.AutoMin, c.AutoMax}, {c.HeatMin, c.HeatMax}} {
		if p[0] > 0 && p[0] < min {
			min = p[0]
		}
		if p[1] > max {
			max = p[1]
		}
	}
	if min >= max { // unplausibel -> Fallback
		return 16, 30
	}
	return min, max
}
