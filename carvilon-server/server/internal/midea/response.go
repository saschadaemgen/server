package midea

import "errors"

const responseState = 0xC0 // Report-Frame mit Ist-Zustand

// State ist der geparste Ist-Zustand eines Klimageräts.
type State struct {
	Power            bool
	Mode             OperationalMode
	TargetTemp       float64 // °C
	FanSpeed         byte    // 0..100 oder 102=auto
	SwingVertical    bool
	SwingHorizontal  bool
	Eco              bool
	Turbo            bool
	Sleep            bool
	Fahrenheit       bool
	FreezeProtection bool
	IndoorTemp       float64 // Raumtemperatur (room_c), 0 wenn n/a
	HasIndoorTemp    bool
	OutdoorTemp      float64 // Außen-/Wärmetauschertemperatur, 0 wenn n/a
	HasOutdoorTemp   bool
}

// frameBody streift 0xAA-Header (10 Byte) und die abschließende Prüfsumme ab.
func frameBody(frame []byte) ([]byte, error) {
	if len(frame) < 12 || frame[0] != 0xAA {
		return nil, errors.New("midea: kein gültiger 0xAA-Frame")
	}
	return frame[10 : len(frame)-1], nil
}

// parseTemperature dekodiert eine Temperatur mit optionaler Nachkommastelle.
func parseTemperature(data byte, decimals float64, fahrenheit bool) (float64, bool) {
	if data == 0xFF {
		return 0, false
	}
	temp := (float64(data) - 50) / 2
	if !fahrenheit && decimals != 0 {
		base := float64(int(temp))
		if temp >= 0 {
			return base + decimals, true
		}
		return base - decimals, true
	}
	if decimals >= 0.5 {
		base := float64(int(temp))
		if temp >= 0 {
			return base + 0.5, true
		}
		return base - 0.5, true
	}
	return temp, true
}

// ParseState liest einen Report-Frame (0xC0). Liefert ErrNotState, wenn der
// Frame kein Zustandsbericht ist (z. B. Bestätigungsframe).
func ParseState(frame []byte) (*State, error) {
	body, err := frameBody(frame)
	if err != nil {
		return nil, err
	}
	if len(body) < 16 || body[0] != responseState {
		return nil, ErrNotState
	}
	p := body // p[0] = 0xC0

	s := &State{}
	s.Power = p[1]&0x1 != 0

	s.TargetTemp = float64(p[2]&0xF) + 16.0
	if p[2]&0x10 != 0 {
		s.TargetTemp += 0.5
	}
	s.Mode = OperationalMode((p[2] >> 5) & 0x7)

	s.FanSpeed = p[3] & 0x7F

	swing := p[7] & 0xF
	s.SwingVertical = swing&0xC != 0
	s.SwingHorizontal = swing&0x3 != 0

	s.Turbo = p[8]&0x20 != 0
	s.Eco = p[9]&0x10 != 0
	s.Sleep = p[10]&0x1 != 0
	s.Turbo = s.Turbo || p[10]&0x2 != 0
	s.Fahrenheit = p[10]&0x4 != 0

	if len(p) > 15 {
		decimals := float64(p[15]&0xF) / 10
		if t, ok := parseTemperature(p[11], decimals, s.Fahrenheit); ok {
			s.IndoorTemp = t
			s.HasIndoorTemp = true
		}
		outDecimals := float64(p[15]>>4) / 10
		if t, ok := parseTemperature(p[12], outDecimals, s.Fahrenheit); ok {
			s.OutdoorTemp = t
			s.HasOutdoorTemp = true
		}
	}
	return s, nil
}

// ErrNotState signalisiert, dass ein Frame kein 0xC0-Zustandsbericht ist.
var ErrNotState = errors.New("midea: Frame ist kein Zustandsbericht")
