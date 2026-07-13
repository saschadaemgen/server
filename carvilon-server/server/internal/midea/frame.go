package midea

import "math"

// Geräte- und Frame-Typen des Midea-Protokolls.
const (
	deviceTypeAC  byte = 0xAC // Klimagerät
	frameControl  byte = 0x02 // Steuerbefehl
	frameQuery    byte = 0x03 // Statusabfrage
	controlSource byte = 0x02 // "App control"
)

// OperationalMode entspricht den vom Gerät erwarteten Moduswerten.
type OperationalMode byte

const (
	ModeAuto    OperationalMode = 1
	ModeCool    OperationalMode = 2
	ModeDry     OperationalMode = 3
	ModeHeat    OperationalMode = 4
	ModeFanOnly OperationalMode = 5
)

// SwingMode steuert die Lamellen.
type SwingMode byte

const (
	SwingOff        SwingMode = 0x0
	SwingVertical   SwingMode = 0xC
	SwingHorizontal SwingMode = 0x3
	SwingBoth       SwingMode = 0xF
)

// laufende Message-ID (1 Byte, wie in der Referenz global hochgezählt).
var messageID byte

func nextMessageID() byte {
	messageID++
	return messageID
}

// buildFrame baut den äußeren 0xAA-Frame (10-Byte-Header + data + Prüfsumme).
func buildFrame(frameType byte, data []byte) []byte {
	header := make([]byte, 10)
	header[0] = 0xAA
	header[1] = byte(len(data) + 10)
	header[2] = deviceTypeAC
	header[8] = 0 // Protokollversion
	header[9] = frameType

	frame := append(header, data...)
	// Prüfsumme über alles ab Index 1: (~sum + 1) & 0xFF
	var sum int
	for _, b := range frame[1:] {
		sum += int(b)
	}
	frame = append(frame, byte((^sum+1)&0xFF))
	return frame
}

// wrapCommand hängt Message-ID + CRC8 an den Body und packt ihn in den Frame.
func wrapCommand(frameType byte, body []byte) []byte {
	payload := append(append([]byte{}, body...), nextMessageID())
	payload = append(payload, crc8(payload))
	return buildFrame(frameType, payload)
}

// SetState beschreibt den gewünschten Zielzustand des Klimageräts.
type SetState struct {
	Power       bool
	Mode        OperationalMode
	TargetTemp  float64 // in °C, z. B. 21 oder 20.5
	FanSpeed    byte    // 0..100, oder 102=auto
	Swing       SwingMode
	Eco         bool
	Turbo       bool
	Sleep       bool
	Beep        bool
	Fahrenheit  bool // Anzeige am Gerät in °F
	FreezeGuard bool // Frostschutz (min. Heizen)
}

// encodeSetState erzeugt den kompletten CONTROL-Frame aus dem Zielzustand.
func encodeSetState(s SetState) []byte {
	var beep, power byte
	if s.Beep {
		beep = 0x40
	}
	if s.Power {
		power = 0x01
	}

	// Zieltemperatur in ganzzahligen + halben Anteil zerlegen.
	// math.Modf liefert (Ganzzahlteil, Nachkommateil) – in dieser Reihenfolge.
	integ, frac := math.Modf(s.TargetTemp)
	it := int(integ)
	var temperature, temperatureAlt byte
	if it >= 17 && it <= 30 {
		temperature = byte((it - 16) & 0xF)
	} else {
		temperatureAlt = byte((it - 12) & 0x1F)
	}
	if frac > 0 {
		temperature |= 0x10 // Halbgrad-Bit
	}

	mode := byte((byte(s.Mode) & 0x7) << 5)
	swingByte := byte(0x30 | (byte(s.Swing) & 0x3F))

	var eco byte
	if s.Eco {
		eco = 0x80
	}
	var sleep, turbo, fahrenheit byte
	if s.Sleep {
		sleep = 0x01
	}
	if s.Turbo {
		turbo = 0x02
	}
	if s.Fahrenheit {
		fahrenheit = 0x04
	}
	var turboAlt byte
	if s.Turbo {
		turboAlt = 0x20
	}
	var freeze byte
	if s.FreezeGuard {
		freeze = 0x80
	}

	body := []byte{
		0x40,                         // Set-State-Marker
		controlSource | beep | power, // Quelle, Beep, Power
		temperature | mode,           // Temperatur + Betriebsmodus
		s.FanSpeed,                   // Lüfterstufe
		0x7F, 0x7F, 0x00,             // Timer (aus)
		swingByte,                  // Swing
		turboAlt,                   // Follow-me | alternatives Turbo
		eco,                        // ECO | Purifier | Aux-Heat
		sleep | turbo | fahrenheit, // Sleep | Turbo | Fahrenheit
		0x00, 0x00, 0x00, 0x00,     // reserviert
		0x00, 0x00, 0x00, // reserviert
		temperatureAlt, // alternative Temperatur
		0x28,           // Zielfeuchte (0x28 = 40, Default)
		0x00,           // reserviert
		freeze,         // Frostschutz
		0x00,           // unabhängiges Aux-Heat
		0x00,           // reserviert
	}

	return wrapCommand(frameControl, body)
}

// encodeQueryState erzeugt den QUERY-Frame zum Statusabruf (GetStateCommand).
// Antwort ist ein 0xC0-REPORT-Frame.
func encodeQueryState() []byte {
	body := []byte{
		0x41, // Get state
		0x81, 0x00, 0xFF, 0x03, 0xFF, 0x00,
		0x02,                   // Temperaturtyp INDOOR
		0x00, 0x00, 0x00, 0x00, // reserviert
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x03,
	}
	return wrapCommand(frameQuery, body)
}

// encodeQueryCapabilities erzeugt den 0xB5-Fähigkeits-Query-Frame.
func encodeQueryCapabilities() []byte {
	return wrapCommand(frameQuery, []byte{0xB5, 0x01, 0x00})
}

// BuildSetFrameForDebug exportiert encodeSetState für Byte-Vergleiche.
func BuildSetFrameForDebug(s SetState) []byte { return encodeSetState(s) }
