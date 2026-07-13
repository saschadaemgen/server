// Package mideaclimate ist der CARVILON-Modul-Controller fuer Midea/Springer-
// Splitklimageraete. Er ist ein Logik-Editor-Baustein (control_loop): CARVILON
// ruft Tick pro Engine-Takt auf, der State lebt in der Instanz.
//
// Regelstrategie (vollstaendig gemessen, siehe SEASON-1-PROTOKOLL.md):
//   - Zwei-Gang-Verdichter: HOCH = Delta 4.0, HALTEN = Delta ~1.5 relativ zum
//     GERAETEFUEHLER (dessen Wert bestimmt die interne Modulation).
//   - Luefter als Feinventil: high herankuehlen, mid ziehen, low halten (Enum
//     auto/low/mid/high; das Geraet kennt keine 0-100-Stufung).
//   - Istwert = externer Sensor (verdrahteter Eingang), geglaettet.
//   - Symmetrisch proportionales Halten + schwacher I-Anteil (Anti-Windup),
//     Tendenz-Uebergabe, Taupunkt-Luefteruntergrenze, Leerlauf mit Puls,
//     Sollwert-Boden gegen den unter Last kollabierenden Geraetefuehler.
//
// Zwei Bezugssysteme: gestellt wird relativ zum Geraetefuehler (aus dem eigenen
// Modul-Status), der Erfolg wird am externen Sensor gemessen.
package mideaclimate

import (
	"math"
	"time"
)

// FanMode ist die Luefter-Vorwahl des Geraets (kein 0-100).
type FanMode string

const (
	FanAuto FanMode = "auto"
	FanLow  FanMode = "low"  // gemessen ~40
	FanMid  FanMode = "mid"  // gemessen ~60
	FanHigh FanMode = "high" // gemessen ~100
)

// Mode ist der Betriebsmodus, der an das Geraet geht.
type Mode string

const (
	ModeOff     Mode = "off"
	ModeCool    Mode = "cool"
	ModeHeat    Mode = "heat"
	ModeDry     Mode = "dry"
	ModeFanOnly Mode = "fan_only"
	// ModeAuto lets the device pick heat/cool itself. The server control loop
	// does not emit it (it drives an explicit gear), but the standard profile
	// exposes it as a remote-like passthrough mode.
	ModeAuto Mode = "auto"
)

// Gang ist der interne Regelzustand.
type Gang int

const (
	GangAus Gang = iota
	GangHalten
	GangHoch
)

func (g Gang) String() string {
	switch g {
	case GangHalten:
		return "HALTEN"
	case GangHoch:
		return "HOCH"
	default:
		return "AUS"
	}
}

// Profile ist das Anwendungsprofil (bestimmt Zielgroessen und Prioritaeten).
type Profile string

const (
	ProfKomfort      Profile = "komfort"      // Temperatur fuehrend
	ProfKultivierung Profile = "kultivierung" // Temp + Feuchte/VPD, Lampen-FF
	ProfBuero        Profile = "buero"        // Temperatur, Zeitplan-orientiert
	ProfHeizen       Profile = "heizen"       // heat-Modus, Delta-Strategie gespiegelt
)

// Params sind die Regel-Stellschrauben. Defaults stammen aus der Messung; jede
// ist ueber die Modul-Settings (SetConfig) ueberschreibbar. Der Automatik-Modus
// belaesst nicht ueberschriebene Werte auf diesen Defaults und laesst die
// adaptive Nachfuehrung wirken.
type Params struct {
	Totband         float64 // +/- C um das Ziel
	HochAb          float64 // Abweichung, ab der HOCH faehrt
	Wiederanlauf    float64 // Abweichung, ab der aus AUS wieder angefahren wird
	DeltaHalten     float64 // Basis-Halte-Delta unter Geraetefuehler
	DeltaHoch       float64 // Hoch-Delta unter Geraetefuehler
	SollBoden       float64 // Sollwert nie tiefer als Ziel - SollBoden
	IGain           float64 // Integral: Delta-Zuwachs pro C*min (0 = aus)
	IMax            float64 // Anti-Windup: max. Integral-Beitrag in C
	TendenzSchwelle float64 // C/min Falltempo fuer HOCH-Uebersprung / frueher Aus
	TaupunktAbstand float64 // Mindestabstand zu Taupunkt in C
	FanFeuchtMin    FanMode // Luefter-Untergrenze nahe Taupunkt
	Glaettung       int     // Fenstergroesse gleitender Mittelwert
	MinGangzeit     time.Duration
	Nachlauf        time.Duration // Trocken-Nachlauf nach Wechsel in AUS
	PulsIntervall   time.Duration // Abstand der Leerlauf-Luefterpulse
	FanHoch         FanMode
	FanZiehen       FanMode
	FanHalten       FanMode
	AusFanOnly      bool // unter Ziel Nur-Luefter statt komplett aus

	// --- Profil / High-Performance-Erweiterungen ---
	Profile        Profile // Anwendungsprofil
	FeuchteMax     float64 // rF-Obergrenze (%), darueber Entfeuchtung (0 = aus)
	VpdZiel        float64 // Ziel-VPD (kPa), 0 = aus (Kultivierung)
	VpdBand        float64 // VPD-Totband (kPa)
	DryFanMin      FanMode // Mindestluefter im dry-Modus
	LampFeedfwd    bool    // Lampen-Feedforward aktiv (Kultivierung)
	LampVorlaufS   float64 // Sekunden Vorkonditionierung vor Licht-an
	LampBoostDelta float64 // zusaetzliches Halte-Delta waehrend Vorlauf (Kondensationsschutz)
	Heizen         bool    // Heizmodus: Delta-Strategie gespiegelt (Ziel liegt oben)
}

// DefaultParams liefert die gemessenen Startwerte (09.07.2026).
func DefaultParams() Params {
	return Params{
		Totband: 0.3, HochAb: 0.8, Wiederanlauf: 0.3,
		DeltaHalten: 1.5, DeltaHoch: 4.0, SollBoden: 2.0,
		IGain: 0.03, IMax: 0.5, TendenzSchwelle: 0.05,
		TaupunktAbstand: 3.0, FanFeuchtMin: FanMid,
		Glaettung: 4, MinGangzeit: 3 * time.Minute,
		Nachlauf: 3 * time.Minute, PulsIntervall: 5 * time.Minute,
		FanHoch: FanHigh, FanZiehen: FanMid, FanHalten: FanLow,
		AusFanOnly: true,
		Profile:    ProfKomfort,
		FeuchteMax: 0, VpdZiel: 0, VpdBand: 0.2, DryFanMin: FanMid,
		LampFeedfwd: false, LampVorlaufS: 1800, LampBoostDelta: 0.5,
		Heizen: false,
	}
}

// Inputs ist der pro Tick von der Engine gelieferte Satz.
type Inputs struct {
	Now         time.Time // Engine-Zeit dieses Ticks
	RoomTemp    float64   // externer Sensor (Regelgroesse), verdrahtet
	RoomHum     float64   // externe Feuchte (0 = n/a), verdrahtet
	HasHum      bool
	Target      float64 // Regelziel
	Enable      bool
	DeviceTemp  float64 // geraeteeigener Rueckluftfuehler (aus Modul-Status)
	HasDevice   bool    // Modul-Status verfuegbar?
	SensorValid bool    // externer Sensor lieferte einen Wert

	// Lampen-Feedforward (Kultivierung): Vorwissen ueber den Lastsprung.
	LightOn    bool    // Licht aktuell an
	LightInS   float64 // Sekunden bis Licht-an (0 = unbekannt/aus)
	HasLightFF bool    // Feedforward-Signale verdrahtet
}

// Outputs ist die Stellentscheidung an das eigene Geraet.
type Outputs struct {
	Send     bool // false = nichts aendern
	Mode     Mode
	Setpoint float64
	Fan      FanMode
}

// Readouts sind die Editor-/Cockpit-Anzeigen (kein Stelleingriff).
type Readouts struct {
	Gang       string
	Abweichung float64
	Tendenz    float64
	Taupunkt   float64
	VPD        float64
	CoolRate   float64
	ITerm      float64
	Alarm      string
}

// Controller haelt den gesamten Regel-State einer Instanz.
type Controller struct {
	P Params

	window     []float64
	current    Gang
	lastChange time.Time
	lastSent   float64
	lastFan    FanMode
	lastMode   Mode
	iTerm      float64
	offSince   time.Time
	lastPulse  time.Time
	idleFanOn  bool
	first      bool
	sensorDown time.Time
	failsafe   bool

	// Kuehlraten-Schaetzung (adaptive Kennlinie, einfache Form).
	lastTemp     float64
	lastTempTime time.Time
	coolRate     float64 // gleitend, C/min unter Last (negativ = kuehlend)
}

// New erstellt einen Controller mit gegebenen Parametern.
func New(p Params) *Controller {
	c := &Controller{P: p}
	c.Reset()
	return c
}

// Init setzt Parameter (Modul-Settings) und initialisiert den State.
func (c *Controller) Init(p Params) { c.P = p; c.Reset() }

// Reset loescht den Laufzeit-State (z. B. nach Sensor-Rueckkehr).
func (c *Controller) Reset() {
	c.window = c.window[:0]
	c.current = GangAus
	c.lastChange = time.Time{}
	c.lastSent = -1
	c.lastFan = ""
	c.lastMode = ""
	c.iTerm = 0
	c.offSince = time.Time{}
	c.lastPulse = time.Time{}
	c.idleFanOn = false
	c.first = true
	c.sensorDown = time.Time{}
	c.failsafe = false
}

// fanFloorRank ordnet FanMode fuer den Taupunkt-Vergleich (auto ausgenommen).
func fanRank(f FanMode) int {
	switch f {
	case FanLow:
		return 1
	case FanMid:
		return 2
	case FanHigh:
		return 3
	default:
		return 0 // auto
	}
}

// VPD (Dampfdruckdefizit, kPa) aus Temperatur und rel. Feuchte.
func VPD(tempC, rhPct float64) float64 {
	if rhPct <= 0 {
		return 0
	}
	svp := 0.6108 * math.Exp(17.27*tempC/(tempC+237.3)) // kPa
	return svp * (1 - rhPct/100)
}

// Taupunkt (Magnus) aus Temperatur und rel. Feuchte.
func Taupunkt(tempC, rhPct float64) float64 {
	if rhPct <= 0 {
		return math.Inf(-1)
	}
	const a, b = 17.62, 243.12
	g := (a*tempC)/(b+tempC) + math.Log(rhPct/100)
	return b * g / (a - g)
}

// Tick fuehrt einen Regelschritt aus. Die Engine liefert Zeit und Eingaenge,
// bekommt eine Stellentscheidung und Anzeigen zurueck. Der State bleibt in c.
func (c *Controller) Tick(in Inputs, dtMin float64) (Outputs, Readouts) {
	var out Outputs
	var rd Readouts

	// Enable aus: sauber ausschalten, State ruhen lassen.
	if !in.Enable {
		if c.current != GangAus || c.first {
			out = Outputs{Send: true, Mode: ModeOff, Setpoint: in.Target, Fan: FanAuto}
			c.current = GangAus
			c.first = false
		}
		rd.Gang = "AUS(disabled)"
		return out, rd
	}

	// Sensorausfall: Karenz, dann konservativer Original-Betrieb + Alarm.
	if !in.SensorValid {
		if c.sensorDown.IsZero() {
			c.sensorDown = in.Now
		}
		if in.Now.Sub(c.sensorDown) >= 5*time.Minute && !c.failsafe {
			c.failsafe = true
			c.current = GangHalten
			c.lastSent, c.lastFan, c.lastMode = in.Target, FanAuto, ModeCool
			rd.Alarm = "Sensorausfall: Fallback auf Original-Betrieb"
			return Outputs{Send: true, Mode: ModeCool, Setpoint: in.Target, Fan: FanAuto}, rd
		}
		rd.Gang = c.current.String()
		rd.Alarm = "Sensor unsicher"
		return Outputs{Send: false}, rd
	}
	if c.failsafe {
		c.failsafe = false
		c.Reset()
	}
	c.sensorDown = time.Time{}

	// Glaettung + Abweichung.
	c.window = append(c.window, in.RoomTemp)
	if len(c.window) > c.P.Glaettung {
		c.window = c.window[len(c.window)-c.P.Glaettung:]
	}
	smooth := mean(c.window)

	// Effektives Ziel: Lampen-Feedforward zieht das Ziel VOR Licht-an bereits
	// leicht in Richtung Tagbetrieb bzw. haelt strenger, um den Feuchte-/
	// Kondensationssprung beim Einschalten abzufangen (Kultivierung).
	effTarget := in.Target
	ffActive := false
	if c.P.LampFeedfwd && in.HasLightFF {
		if !in.LightOn && in.LightInS > 0 && in.LightInS <= c.P.LampVorlaufS {
			// Vorkonditionierung: Raum vorab naeher ans Ziel bringen (verhindert
			// den steilen Sprung auf kalte Oberflaechen -> Schimmelrisiko).
			ffActive = true
		}
	}

	abw := smooth - effTarget

	// Heizmodus: Vorzeichen der Regelung spiegeln (Ziel liegt oben, "zu kalt"
	// statt "zu warm" treibt HOCH). Wir invertieren die Abweichung fuer die
	// Gang-/Delta-Logik und drehen den Modus.
	heat := c.P.Heizen || c.P.Profile == ProfHeizen
	ctrlAbw := abw
	if heat {
		ctrlAbw = -abw
	}

	// Kuehlraten-Schaetzung (adaptive Kennlinie, gleitend).
	if !c.lastTempTime.IsZero() {
		dtm := in.Now.Sub(c.lastTempTime).Minutes()
		if dtm > 0.1 {
			rate := (smooth - c.lastTemp) / dtm
			c.coolRate = 0.7*c.coolRate + 0.3*rate
		}
	}
	c.lastTemp, c.lastTempTime = smooth, in.Now

	// Tendenz (C/min).
	tendenz := 0.0
	if len(c.window) >= 2 && dtMin > 0 {
		tendenz = (c.window[len(c.window)-1] - c.window[0]) / (float64(len(c.window)-1) * dtMin)
	}

	// Bezugsgroesse fuer den Sollwert: Geraetefuehler, sonst Fallback auf Istwert.
	ref := in.RoomTemp
	if in.HasDevice {
		ref = in.DeviceTemp
	}

	// Gangwahl (Hysterese, Wiederanlauf, Tendenz, Mindestgangzeit).
	// ctrlAbw ist im Kuehlbetrieb = abw, im Heizbetrieb = -abw (gespiegelt),
	// so dass die gleiche Logik "zu weit weg -> HOCH" fuer beide gilt.
	want := c.current
	switch {
	case ctrlAbw > c.P.HochAb:
		want = GangHoch
		if tendenz != 0 && ((!heat && tendenz <= -c.P.TendenzSchwelle) || (heat && tendenz >= c.P.TendenzSchwelle)) {
			want = GangHalten // naehert sich schon schnell -> HOCH ueberspringen
		}
	case ctrlAbw > c.P.Totband:
		want = GangHalten
	case ctrlAbw < -c.P.Totband:
		want = GangAus
	default:
		if c.current == GangHoch {
			want = GangHalten
		}
	}
	if c.current == GangAus && want != GangAus && ctrlAbw <= c.P.Wiederanlauf {
		want = GangAus
	}
	// Ueberschwungschutz: schon jenseits des Ziels und weiter in die falsche
	// Richtung -> frueher abschalten.
	if c.current == GangHalten && ctrlAbw < 0 {
		if (!heat && tendenz < -0.01) || (heat && tendenz > 0.01) {
			want = GangAus
		}
	}
	if want != c.current && !c.lastChange.IsZero() && in.Now.Sub(c.lastChange) < c.P.MinGangzeit {
		want = c.current
	}

	// Integral-Anteil (nur im HALTEN aufziehen), Vorzeichen gemaess ctrlAbw.
	if c.current == GangHalten {
		c.iTerm = clamp(c.iTerm+ctrlAbw*dtMin*c.P.IGain, -c.P.IMax, c.P.IMax)
	} else {
		c.iTerm *= 0.9
	}

	// Feedforward-Zusatzdelta waehrend der Vorkonditionierung.
	ffDelta := 0.0
	if ffActive {
		ffDelta = c.P.LampBoostDelta
	}

	// Sollwert + Luefter je Gang ableiten. Im Heizbetrieb ist das Delta nach
	// OBEN (ref + delta), sonst nach unten (ref - delta).
	var setp float64
	var fan FanMode
	mode := ModeCool
	sign := 1.0
	if heat {
		mode = ModeHeat
		sign = -1.0 // ref - sign*delta == ref + delta
	}
	switch want {
	case GangHoch:
		setp = clamp(ref-sign*c.P.DeltaHoch, 17, 30)
		fan = c.P.FanHoch
	case GangHalten:
		zug := clamp(ctrlAbw, -(c.P.DeltaHalten - 0.5), c.P.DeltaHoch-c.P.DeltaHalten-0.5)
		delta := clamp(c.P.DeltaHalten+zug+c.iTerm+ffDelta, 0.5, c.P.DeltaHoch-0.5)
		setp = clamp(ref-sign*delta, 17, 30)
		if ctrlAbw > c.P.Totband {
			fan = c.P.FanZiehen
		} else {
			fan = c.P.FanHalten
		}
	}

	// Sollwert-Boden gegen kollabierenden Geraetefuehler (Kuehlen: Boden nach
	// unten; Heizen: symmetrisch Decke nach oben).
	if want != GangAus {
		if !heat && setp < in.Target-c.P.SollBoden {
			setp = in.Target - c.P.SollBoden
		}
		if heat && setp > in.Target+c.P.SollBoden {
			setp = in.Target + c.P.SollBoden
		}
	}

	// VPD + Taupunkt.
	tp := math.Inf(-1)
	vpd := 0.0
	if in.HasHum {
		tp = Taupunkt(smooth, in.RoomHum)
		vpd = VPD(smooth, in.RoomHum)
		// Taupunkt-Schutz: nahe Taupunkt keine niedrige Luefterstufe beim Kuehlen.
		if want != GangAus && !heat && smooth-tp < c.P.TaupunktAbstand &&
			fan != FanAuto && fanRank(fan) < fanRank(c.P.FanFeuchtMin) {
			fan = c.P.FanFeuchtMin
		}
	}

	// Feuchte-Override: zu feucht und Temperatur im/unter Band -> dry-Modus.
	// dry entfeuchtet gezielt; Mindestluefter schuetzt vor Vereisung.
	dryOverride := false
	if in.HasHum && c.P.FeuchteMax > 0 && in.RoomHum > c.P.FeuchteMax && ctrlAbw <= c.P.Totband {
		dryOverride = true
	}
	if in.HasHum && c.P.VpdZiel > 0 && vpd < c.P.VpdZiel-c.P.VpdBand && ctrlAbw <= c.P.Totband {
		dryOverride = true
	}
	if dryOverride {
		mode = ModeDry
		want = GangHalten // dry als aktiver Zustand
		if fanRank(fan) < fanRank(c.P.DryFanMin) && fan != FanAuto {
			fan = c.P.DryFanMin
		}
		if setp == 0 {
			setp = in.Target
		}
	}

	setp = math.Round(setp*2) / 2

	// Leerlauf-Substatus: Trocken-Nachlauf, danach Luefter-Puls (Kondensat).
	wantIdleFan := false
	if want == GangAus && c.P.AusFanOnly {
		if c.current != GangAus || c.first {
			c.offSince = in.Now
			c.lastPulse = in.Now
		}
		switch {
		case in.Now.Sub(c.offSince) < c.P.Nachlauf:
			wantIdleFan = true
		case in.Now.Sub(c.lastPulse) >= c.P.PulsIntervall:
			wantIdleFan = true
			c.lastPulse = in.Now
		}
	}

	// Nur senden bei Aenderung (oder erstem Tick).
	send := c.first || want != c.current ||
		(want != GangAus && (math.Abs(setp-c.lastSent) >= 0.5 || fan != c.lastFan)) ||
		(want == GangAus && c.P.AusFanOnly && wantIdleFan != c.idleFanOn)

	if send {
		if want == GangAus {
			if c.P.AusFanOnly && wantIdleFan {
				out = Outputs{Send: true, Mode: ModeFanOnly, Setpoint: in.Target, Fan: c.P.FanHalten}
			} else {
				out = Outputs{Send: true, Mode: ModeOff, Setpoint: in.Target, Fan: FanAuto}
			}
		} else {
			out = Outputs{Send: true, Mode: mode, Setpoint: setp, Fan: fan}
		}
		if want != c.current {
			c.lastChange = in.Now
		}
		c.current = want
		c.idleFanOn = want == GangAus && c.P.AusFanOnly && wantIdleFan
		if want != GangAus {
			c.lastSent, c.lastFan, c.lastMode = setp, fan, mode
		}
		c.first = false
	}

	rd = Readouts{
		Gang: c.current.String(), Abweichung: abw, Tendenz: tendenz,
		Taupunkt: tp, VPD: vpd, CoolRate: c.coolRate, ITerm: c.iTerm,
	}
	return out, rd
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
