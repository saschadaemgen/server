// Adapter: duenner Geraetezugriff fuer die Midea/Springer-Splitgeraete.
// Keine Steuerstrategie (die lebt im Controller), keine fest verdrahteten
// Credentials (die kommen ueber Provision). Bildet die Modul-Capabilities
// setpoint/mode/fan_mode/sensor auf das native midea-Protokoll ab.
package mideaclimate

import (
	"context"
	"fmt"
	"time"

	"carvilon.local/server/internal/midea"
)

// Credentials kommen ausschliesslich ueber Provision vom CARVILON-Server
// (verschluesselt gespeichert), niemals aus dem Quellcode.
type Credentials struct {
	IP       string
	DeviceID uint64
	Token    []byte
	Key      []byte
}

// Device ist eine adoptierte Geraete-Instanz.
type Device struct {
	Addr  string
	creds Credentials
	ac    *midea.AirConditioner
}

// State sind die live gelesenen Werte der deklarierten Capabilities.
type State struct {
	Power       bool
	Mode        Mode
	Setpoint    float64
	Fan         FanMode
	DeviceTempC float64 // Rueckluftfuehler (sensor-Capability)
	HasTemp     bool
	OutdoorC    float64
	HasOutdoor  bool
}

// fanToMidea bildet das Modul-Enum auf die gemessenen Geraetestufen ab.
func fanToMidea(f FanMode) byte {
	switch f {
	case FanLow:
		return 40
	case FanMid:
		return 60
	case FanHigh:
		return 100
	default:
		return 102 // auto
	}
}

// fanFromMidea bildet die Geraetestufe zurueck auf das Enum (Leseschwellen).
func fanFromMidea(v byte) FanMode {
	switch {
	case v == 102:
		return FanAuto
	case v >= 70:
		return FanHigh
	case v >= 40:
		return FanMid
	default:
		return FanLow
	}
}

func modeToMidea(m Mode) midea.OperationalMode {
	switch m {
	case ModeCool:
		return midea.ModeCool
	case ModeHeat:
		return midea.ModeHeat
	case ModeDry:
		return midea.ModeDry
	case ModeFanOnly:
		return midea.ModeFanOnly
	case ModeAuto:
		return midea.ModeAuto
	default:
		return midea.ModeCool
	}
}

func modeFromMidea(m midea.OperationalMode, power bool) Mode {
	if !power {
		return ModeOff
	}
	switch m {
	case midea.ModeHeat:
		return ModeHeat
	case midea.ModeDry:
		return ModeDry
	case midea.ModeFanOnly:
		return ModeFanOnly
	case midea.ModeAuto:
		return ModeAuto
	default:
		return ModeCool
	}
}

// Provision adoptiert ein Geraet mit vom Server gelieferten Credentials.
func Provision(ctx context.Context, addr string, creds Credentials) (*Device, error) {
	ac, err := midea.NewAirConditioner(creds.IP, creds.DeviceID, fmt.Sprintf("%x", creds.Token), fmt.Sprintf("%x", creds.Key))
	if err != nil {
		return nil, err
	}
	d := &Device{Addr: addr, creds: creds, ac: ac}
	if err := d.connect(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Device) connect(ctx context.Context) error {
	type res struct{ err error }
	ch := make(chan res, 1)
	go func() { ch <- res{d.ac.Connect()} }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		return r.err
	}
}

// Deprovision trennt die Verbindung.
func (d *Device) Deprovision(ctx context.Context) error {
	if d.ac != nil {
		return d.ac.Close()
	}
	return nil
}

// Status liest die aktuellen Werte (sensor + control-Rueckmeldung).
func (d *Device) Status(ctx context.Context) (State, error) {
	st, err := d.queryCtx(ctx)
	if err != nil {
		return State{}, err
	}
	s := State{
		Power:    st.Power,
		Mode:     modeFromMidea(st.Mode, st.Power),
		Setpoint: st.TargetTemp,
		Fan:      fanFromMidea(st.FanSpeed),
	}
	if st.HasIndoorTemp {
		s.DeviceTempC, s.HasTemp = st.IndoorTemp, true
	}
	if st.HasOutdoorTemp {
		s.OutdoorC, s.HasOutdoor = st.OutdoorTemp, true
	}
	return s, nil
}

// Control fuehrt eine Stellentscheidung des Controllers aus (setpoint/mode/fan).
func (d *Device) Control(ctx context.Context, out Outputs) error {
	if !out.Send {
		return nil
	}
	cmd := midea.SetState{
		Power:      out.Mode != ModeOff,
		Mode:       modeToMidea(out.Mode),
		TargetTemp: out.Setpoint,
		FanSpeed:   fanToMidea(out.Fan),
		Beep:       false,
	}
	type res struct{ err error }
	ch := make(chan res, 1)
	go func() { _, e := d.ac.ApplyAndRead(cmd); ch <- res{e} }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		return r.err
	}
}

func (d *Device) queryCtx(ctx context.Context) (*midea.State, error) {
	type res struct {
		st  *midea.State
		err error
	}
	ch := make(chan res, 1)
	go func() { st, e := d.ac.Query(); ch <- res{st, e} }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.st, r.err
	}
}

// --- Standard-Profil: direkte Bedienung wie Fernbedienung -------------------
// Im Standard-Profil (execution=device) gibt es keinen Regelkreis. Der Nutzer
// setzt Zieltemperatur, Modus und Luefter direkt; die Anlage regelt selbst auf
// ihren internen Fuehler. Diese API bildet genau die Fernbedienungs-Funktionen
// ab und nutzt denselben Adapter/dieselbe Protokoll-Schicht wie das erweiterte
// Profil - kein duplizierter Code.

// SetTemperature setzt die Zieltemperatur (0,5-Schritt, geraeteseitig geregelt).
func (d *Device) SetTemperature(ctx context.Context, tempC float64) error {
	cur, err := d.Status(ctx)
	if err != nil {
		return err
	}
	return d.Control(ctx, Outputs{Send: true, Mode: nonOff(cur.Mode), Setpoint: tempC, Fan: cur.Fan})
}

// SetMode schaltet den Betriebsmodus (off/cool/heat/dry/fan_only).
func (d *Device) SetMode(ctx context.Context, m Mode) error {
	cur, err := d.Status(ctx)
	if err != nil {
		return err
	}
	sp := cur.Setpoint
	if sp < 17 || sp > 30 {
		sp = 24
	}
	return d.Control(ctx, Outputs{Send: true, Mode: m, Setpoint: sp, Fan: cur.Fan})
}

// SetFan waehlt die Luefterstufe (auto/low/mid/high).
func (d *Device) SetFan(ctx context.Context, f FanMode) error {
	cur, err := d.Status(ctx)
	if err != nil {
		return err
	}
	sp := cur.Setpoint
	if sp < 17 || sp > 30 {
		sp = 24
	}
	return d.Control(ctx, Outputs{Send: true, Mode: nonOff(cur.Mode), Setpoint: sp, Fan: f})
}

// nonOff verhindert, dass ein Temperatur-/Luefterbefehl die Anlage ausschaltet:
// stand sie auf off, wird cool angenommen (wie beim Einschalten per Fernbedienung).
func nonOff(m Mode) Mode {
	if m == ModeOff {
		return ModeCool
	}
	return m
}

// StatusWithTimeout ist ein Komfort-Wrapper mit gebundenem Timeout.
func (d *Device) StatusWithTimeout(t time.Duration) (State, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	return d.Status(ctx)
}
