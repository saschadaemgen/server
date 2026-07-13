package mideaclimate

import (
	"testing"
	"time"
)

// simRoom ist ein einfaches thermisches Modell: konstante Last (Drift) plus
// Kuehlung, die vom gewaehlten Gang/Luefter abhaengt. Grob an die Messung
// angelehnt, nur zum Verifizieren des Regelverhaltens (nicht der Absolutwerte).
type simRoom struct {
	temp    float64
	device  float64 // Geraetefuehler (traeg, kollabiert unter Last)
	driftPM float64 // Drift pro Minute
}

func (s *simRoom) step(out Outputs, dtMin float64) {
	cool := 0.0
	if out.Send && out.Mode == ModeCool {
		switch out.Fan {
		case FanHigh:
			cool = 0.21
		case FanMid:
			cool = 0.06
		case FanLow:
			cool = 0.02
		}
		// tieferes Delta -> etwas mehr, aber gesaettigt
	}
	s.temp += (s.driftPM - cool) * dtMin
	// Geraetefuehler naehert sich unter Kuehlung dem (kaelteren) Wert an
	target := s.temp
	if cool > 0.1 {
		target = s.temp - 1.0 // Rezirkulation: misst kaelter
	}
	s.device += (target - s.device) * 0.3
}

func TestControllerEinpendeln(t *testing.T) {
	c := New(DefaultParams())
	room := &simRoom{temp: 27.0, device: 27.5, driftPM: 0.04}
	now := time.Now()
	dt := 30 * time.Second
	dtMin := dt.Minutes()

	minT, maxT := 99.0, 0.0
	var last Outputs
	for i := 0; i < 400; i++ { // 200 Minuten
		now = now.Add(dt)
		in := Inputs{
			Now: now, RoomTemp: room.temp, Target: 25.0, Enable: true,
			DeviceTemp: room.device, HasDevice: true, SensorValid: true,
		}
		out, _ := c.Tick(in, dtMin)
		if out.Send {
			last = out
		}
		room.step(last, dtMin)
		if i > 120 { // nach Einschwingen Band messen
			if room.temp < minT {
				minT = room.temp
			}
			if room.temp > maxT {
				maxT = room.temp
			}
		}
	}
	band := maxT - minT
	t.Logf("eingeschwungenes Band: %.2f C (min %.2f / max %.2f)", band, minT, maxT)
	if band > 1.0 {
		t.Errorf("Band %.2f C zu gross (Ziel < 1.0)", band)
	}
	if minT < 24.0 || maxT > 26.0 {
		t.Errorf("Ausreisser ausserhalb 24..26: min %.2f max %.2f", minT, maxT)
	}
}

func TestSymmetrischesHalten(t *testing.T) {
	// Kernaussage: Bei negativer Abweichung ist das Halte-Delta kleiner als bei
	// positiver. Wir vergleichen den effektiven Sollwert-Abstand zum Referenz-
	// fuehler in beiden Faellen direkt. I-Anteil und Sollboden ausgeschaltet,
	// damit nur der Proportionalteil wirkt.
	p := DefaultParams()
	p.IGain = 0
	p.SollBoden = 10 // praktisch deaktiviert
	p.MinGangzeit = 0

	deltaBei := func(abw float64) float64 {
		c := New(p)
		now := time.Now()
		ref := 26.0
		target := ref - abw // so dass RoomTemp-Target = abw, mit RoomTemp=ref
		var out Outputs
		for i := 0; i < 3; i++ {
			now = now.Add(30 * time.Second)
			o, _ := c.Tick(Inputs{Now: now, RoomTemp: ref, Target: target, Enable: true,
				DeviceTemp: ref, HasDevice: true, SensorValid: true}, 0.5)
			if o.Send {
				out = o
			}
		}
		return ref - out.Setpoint // = wirksames Delta
	}
	dPlus := deltaBei(0.5)   // ueber Ziel
	dMinus := deltaBei(-0.2) // unter Ziel
	t.Logf("Delta ueber Ziel=%.2f, unter Ziel=%.2f", dPlus, dMinus)
	if !(dMinus < dPlus) {
		t.Errorf("symmetrisches Halten verletzt: Delta unter Ziel (%.2f) nicht < ueber Ziel (%.2f)", dMinus, dPlus)
	}
}

func TestSollBoden(t *testing.T) {
	p := DefaultParams()
	c := New(p)
	now := time.Now()
	// Geraetefuehler kollabiert auf 22, Ziel 25, SollBoden 2 -> nie unter 23
	for i := 0; i < 3; i++ {
		now = now.Add(30 * time.Second)
		out, _ := c.Tick(Inputs{Now: now, RoomTemp: 25.9, Target: 25.0, Enable: true,
			DeviceTemp: 22.0, HasDevice: true, SensorValid: true}, 0.5)
		if out.Send && out.Mode == ModeCool && out.Setpoint < 25.0-p.SollBoden {
			t.Errorf("Sollwert-Boden verletzt: %.1f < %.1f", out.Setpoint, 25.0-p.SollBoden)
		}
	}
}

func TestFanMapping(t *testing.T) {
	cases := map[FanMode]byte{FanAuto: 102, FanLow: 40, FanMid: 60, FanHigh: 100}
	for f, want := range cases {
		if got := fanToMidea(f); got != want {
			t.Errorf("fanToMidea(%s)=%d, erwartet %d", f, got, want)
		}
	}
	// Rueckabbildung ueber Schwellen
	if fanFromMidea(102) != FanAuto || fanFromMidea(100) != FanHigh ||
		fanFromMidea(60) != FanMid || fanFromMidea(40) != FanMid || fanFromMidea(20) != FanLow {
		t.Error("fanFromMidea Schwellen falsch")
	}
}

func TestTaupunkt(t *testing.T) {
	// 25 C / 60 % -> ~16.7 C
	tp := Taupunkt(25, 60)
	if tp < 16.0 || tp > 17.5 {
		t.Errorf("Taupunkt(25,60)=%.2f, erwartet ~16.7", tp)
	}
}

func TestSensorFallback(t *testing.T) {
	c := New(DefaultParams())
	now := time.Now()
	// Sensor 6 Minuten ungueltig -> Failsafe muss greifen
	var got Outputs
	for i := 0; i < 14; i++ {
		now = now.Add(30 * time.Second)
		got, _ = c.Tick(Inputs{Now: now, Target: 25.0, Enable: true,
			HasDevice: true, DeviceTemp: 26, SensorValid: false}, 0.5)
	}
	if !c.failsafe {
		t.Error("Failsafe nach Karenz nicht aktiv")
	}
	_ = got
}

func TestHeizmodus(t *testing.T) {
	// Im Heizbetrieb muss "zu kalt" HOCH ausloesen und der Sollwert UEBER dem
	// Referenzfuehler liegen (ref + delta), Modus heat.
	p := DefaultParams()
	p.Heizen = true
	p.MinGangzeit = 0
	c := New(p)
	now := time.Now()
	var out Outputs
	for i := 0; i < 4; i++ {
		now = now.Add(30 * time.Second)
		o, _ := c.Tick(Inputs{Now: now, RoomTemp: 19.0, Target: 22.0, Enable: true,
			DeviceTemp: 19.0, HasDevice: true, SensorValid: true}, 0.5)
		if o.Send {
			out = o
		}
	}
	if out.Mode != ModeHeat {
		t.Errorf("Modus = %s, erwartet heat", out.Mode)
	}
	if out.Setpoint <= 19.0 {
		t.Errorf("Heiz-Sollwert %.1f nicht ueber Referenz 19.0", out.Setpoint)
	}
}

func TestVpdDryOverride(t *testing.T) {
	// Temperatur im Band, aber VPD zu niedrig (zu feucht) -> dry-Modus.
	p := DefaultParams()
	p.Profile = ProfKultivierung
	p.VpdZiel = 1.2
	p.VpdBand = 0.2
	p.MinGangzeit = 0
	c := New(p)
	now := time.Now()
	var out Outputs
	for i := 0; i < 4; i++ {
		now = now.Add(30 * time.Second)
		// 25 C, 80 % rF -> VPD ~0.63 kPa, deutlich unter Ziel 1.2
		o, _ := c.Tick(Inputs{Now: now, RoomTemp: 25.0, RoomHum: 80, HasHum: true,
			Target: 25.0, Enable: true, DeviceTemp: 25.0, HasDevice: true, SensorValid: true}, 0.5)
		if o.Send {
			out = o
		}
	}
	if out.Mode != ModeDry {
		t.Errorf("Modus = %s, erwartet dry (VPD zu niedrig)", out.Mode)
	}
}

func TestVPDWert(t *testing.T) {
	// 25 C / 50 % -> SVP ~3.17 kPa, VPD ~1.58 kPa
	v := VPD(25, 50)
	if v < 1.4 || v > 1.75 {
		t.Errorf("VPD(25,50)=%.2f, erwartet ~1.58", v)
	}
}

func TestNonOff(t *testing.T) {
	if nonOff(ModeOff) != ModeCool {
		t.Error("nonOff(off) sollte cool sein")
	}
	if nonOff(ModeHeat) != ModeHeat {
		t.Error("nonOff(heat) sollte heat bleiben")
	}
}

func TestUdpID(t *testing.T) {
	// Referenz: udpid = XOR der SHA256-Haelften. Deterministischer Selbsttest.
	// Synthetic device-ID (placeholder), little endian.
	import_binary_le := func(v uint64) []byte {
		b := make([]byte, 8)
		for i := 0; i < 8; i++ {
			b[i] = byte(v >> (8 * i))
		}
		return b
	}
	got := udpID(import_binary_le(1234567890123))
	if len(got) != 32 { // 16 Byte hex = 32 Zeichen
		t.Errorf("udpID Laenge = %d, erwartet 32", len(got))
	}
	// Stabilitaet: gleicher Input -> gleicher Output
	if got != udpID(import_binary_le(1234567890123)) {
		t.Error("udpID nicht deterministisch")
	}
}

func TestEncryptPassword(t *testing.T) {
	// sha256(loginId + sha256(pw) + APP_KEY) - Selbstkonsistenz + Laenge.
	h := encryptPassword("login123", "password1")
	if len(h) != 64 {
		t.Errorf("encryptPassword Laenge = %d, erwartet 64", len(h))
	}
	if h == encryptPassword("login124", "password1") {
		t.Error("encryptPassword ignoriert loginId")
	}
}

func TestSignDeterministic(t *testing.T) {
	body := map[string]string{"b": "2", "a": "1", "c": "3"}
	s1 := sign("/v1/user/login", body)
	s2 := sign("/v1/user/login", body)
	if s1 != s2 || len(s1) != 64 {
		t.Errorf("sign nicht deterministisch oder falsche Laenge (%d)", len(s1))
	}
}

func TestExportImportCredentials(t *testing.T) {
	orig := Credentials{
		IP: "192.0.2.73", DeviceID: 1234567890123,
		Token: []byte{0xde, 0xad, 0xbe, 0xef}, Key: []byte{0x01, 0x02, 0x03},
	}
	text := ExportCredentials(orig)
	back, err := ImportCredentialsFromExport(text)
	if err != nil {
		t.Fatalf("Reimport: %v", err)
	}
	if back.IP != orig.IP || back.DeviceID != orig.DeviceID ||
		hexEq(back.Token, orig.Token) == false || hexEq(back.Key, orig.Key) == false {
		t.Errorf("Roundtrip fehlgeschlagen: %+v vs %+v", back, orig)
	}
}

func hexEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
