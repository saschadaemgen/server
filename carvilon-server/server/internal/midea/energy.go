package midea

import "errors"

// encodeQueryEnergy erzeugt die Energie-/Leistungsabfrage ("group data 4").
func encodeQueryEnergy() []byte {
	body := make([]byte, 20)
	body[0] = 0x41
	body[1] = 0x21
	body[2] = 0x01
	body[3] = 0x44
	return wrapCommand(frameQuery, body)
}

// Energy enthält die Energie-/Leistungswerte eines Geräts.
type Energy struct {
	TotalKWh   float64 // Gesamtenergie (kWh)
	CurrentKWh float64 // Energie des aktuellen Laufs (kWh)
	PowerW     float64 // Echtzeit-Leistungsaufnahme (Watt)
	Valid      bool    // false = Gerät liefert keine Energiedaten
}

// ErrNotEnergy signalisiert, dass ein Frame keine Energie-Antwort ist.
var ErrNotEnergy = errors.New("midea: Frame ist keine Energie-Antwort")

func decodeBCD(d byte) int { return 10*int(d>>4) + int(d&0xF) }

// parseEnergyKWh dekodiert 4 BCD-Bytes zu kWh.
func parseEnergyKWh(d []byte) float64 {
	return 10000*float64(decodeBCD(d[0])) +
		100*float64(decodeBCD(d[1])) +
		1*float64(decodeBCD(d[2])) +
		0.01*float64(decodeBCD(d[3]))
}

// parsePowerW dekodiert 3 BCD-Bytes zu Watt.
func parsePowerW(d []byte) float64 {
	return 1000*float64(decodeBCD(d[0])) +
		10*float64(decodeBCD(d[1])) +
		0.1*float64(decodeBCD(d[2]))
}

// ParseEnergy liest eine Energie-Antwort ("group data 4", Body[0]=0xC1).
func ParseEnergy(frame []byte) (*Energy, error) {
	body, err := frameBody(frame)
	if err != nil {
		return nil, err
	}
	// Antworten auf 0x41/0x21/0x01/0x44 kommen als 0xC1-Report.
	if len(body) < 19 || body[0] != 0xC1 {
		return nil, ErrNotEnergy
	}
	e := &Energy{
		TotalKWh:   parseEnergyKWh(body[4:8]),
		CurrentKWh: parseEnergyKWh(body[12:16]),
		PowerW:     parsePowerW(body[16:19]),
	}
	e.Valid = e.TotalKWh != 0 || e.CurrentKWh != 0 || e.PowerW != 0
	return e, nil
}
