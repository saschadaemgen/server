package midea

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// DefaultPort ist der Standard-TCP-Port des Midea-LAN-Protokolls.
const DefaultPort = 6444

// DefaultTimeout gilt für Verbindung, Handshake und Antworten.
const DefaultTimeout = 5 * time.Second

// AirConditioner repräsentiert ein einzelnes V3-Klimagerät im lokalen Netz.
type AirConditioner struct {
	IP       string
	Port     int
	DeviceID uint64
	Token    []byte // 64 Byte
	Key      []byte // 32 Byte

	conn *conn

	// Drained enthält die beim Verbinden abgeräumten unaufgeforderten Frames
	// (z. B. der Status-Push direkt nach dem Login).
	Drained [][]byte

	// mu serialisiert allen Draht-Zugriff auf die eine TCP-Verbindung dieses
	// Geräts. Das Midea-8370-Protokoll multiplext eine einzige Verbindung
	// (Request -> Response, fortlaufende packetID), ist also NICHT nebenläufig
	// sicher: zwei gleichzeitige Kommandos (z. B. Hintergrund-Poll + Steuerbefehl
	// aus dem Cockpit) würden Frames verschränken und die packetID rennen lassen.
	// Alle öffentlichen Methoden nehmen mu; da keine davon eine andere aufruft,
	// gibt es keine Re-Entrancy. Der Zugriff auf conn (Connect/Close) ist damit
	// ebenfalls geschützt, sodass ein Doppel-Close nicht auf conn rennt.
	mu sync.Mutex
}

// NewAirConditioner erstellt einen Client aus Hex-Strings für Token/Key.
// deviceID, token und key stammen aus `msmart-ng discover`.
func NewAirConditioner(ip string, deviceID uint64, tokenHex, keyHex string) (*AirConditioner, error) {
	token, err := hex.DecodeString(tokenHex)
	if err != nil {
		return nil, fmt.Errorf("midea: Token ungültig: %w", err)
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("midea: Key ungültig: %w", err)
	}
	return &AirConditioner{
		IP:       ip,
		Port:     DefaultPort,
		DeviceID: deviceID,
		Token:    token,
		Key:      key,
	}, nil
}

// Connect stellt die Verbindung her und authentifiziert das Gerät.
func (a *AirConditioner) Connect() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := dial(a.IP, a.Port, DefaultTimeout)
	if err != nil {
		return err
	}
	if err := c.authenticate(a.Token, a.Key, DefaultTimeout); err != nil {
		_ = c.close()
		return err
	}
	a.conn = c
	// Unaufgeforderten Status-Push direkt nach dem Login abräumen, damit die
	// erste echte Abfrage nicht diesen erwischt.
	a.Drained = c.drainUnsolicited(700 * time.Millisecond)
	return nil
}

// Close trennt die Verbindung.
func (a *AirConditioner) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return nil
	}
	err := a.conn.close()
	a.conn = nil
	return err
}

// Apply sendet einen Zielzustand an das Gerät.
func (a *AirConditioner) Apply(state SetState) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return fmt.Errorf("midea: nicht verbunden (Connect() zuerst aufrufen)")
	}
	frame := encodeSetState(state)
	_, err := a.conn.sendCommand(a.DeviceID, frame, DefaultTimeout)
	return err
}

// Query fragt den Ist-Zustand ab und liefert ihn geparst zurück.
func (a *AirConditioner) Query() (*State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return nil, fmt.Errorf("midea: nicht verbunden")
	}
	frames, err := a.conn.sendCommand(a.DeviceID, encodeQueryState(), DefaultTimeout)
	if err != nil {
		return nil, err
	}
	for _, f := range frames {
		if st, perr := ParseState(f); perr == nil {
			return st, nil
		}
	}
	return nil, fmt.Errorf("midea: keine auswertbare Zustandsantwort")
}

// ApplyAndRead sendet einen Zielzustand und versucht, die Antwort als neuen
// Ist-Zustand zu parsen (viele Geräte antworten auf CONTROL mit einem 0xC0).
func (a *AirConditioner) ApplyAndRead(state SetState) (*State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return nil, fmt.Errorf("midea: nicht verbunden")
	}
	frames, err := a.conn.sendCommand(a.DeviceID, encodeSetState(state), DefaultTimeout)
	if err != nil {
		return nil, err
	}
	for _, f := range frames {
		if st, perr := ParseState(f); perr == nil {
			return st, nil
		}
	}
	return nil, nil // Kein parsebarer Zustand; Befehl dennoch gesendet.
}

// Connected meldet, ob eine authentifizierte Verbindung besteht.
func (a *AirConditioner) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn != nil
}

// QueryCapabilities fragt die Fähigkeiten des Geräts ab (0xB5).
func (a *AirConditioner) QueryCapabilities() (*Capabilities, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return nil, fmt.Errorf("midea: nicht verbunden")
	}
	frames, err := a.conn.sendCommand(a.DeviceID, encodeQueryCapabilities(), DefaultTimeout)
	if err != nil {
		return nil, err
	}
	for _, f := range frames {
		if caps, perr := ParseCapabilities(f); perr == nil {
			return caps, nil
		}
	}
	return nil, fmt.Errorf("midea: keine auswertbare Fähigkeits-Antwort")
}

// QueryEnergy fragt Energie-/Leistungsdaten ab (sofern das Gerät sie liefert).
func (a *AirConditioner) QueryEnergy() (*Energy, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return nil, fmt.Errorf("midea: nicht verbunden")
	}
	frames, err := a.conn.sendCommand(a.DeviceID, encodeQueryEnergy(), DefaultTimeout)
	if err != nil {
		return nil, err
	}
	for _, f := range frames {
		if e, perr := ParseEnergy(f); perr == nil {
			return e, nil
		}
	}
	return nil, fmt.Errorf("midea: keine auswertbare Energie-Antwort")
}

// --- Debug-Helfer: rohe Frames sichtbar machen ---

// BuildStateQuery liefert den GetState-Frame (0xC0-Antwort erwartet).
func BuildStateQuery() []byte { return encodeQueryState() }

// BuildCapabilitiesQuery liefert den GetCapabilities-Frame (0xB5-Antwort erwartet).
func BuildCapabilitiesQuery() []byte { return encodeQueryCapabilities() }

// BuildCapabilitiesQueryMore liefert die zweite Capability-Variante (0xB5 01 01 01).
func BuildCapabilitiesQueryMore() []byte {
	return wrapCommand(frameQuery, []byte{0xB5, 0x01, 0x01, 0x01})
}

// SendRaw sendet einen fertigen 0xAA-Appliance-Frame und liefert die rohen,
// dekodierten Antwort-Frames zurück (für Diagnose/Reverse-Engineering).
func (a *AirConditioner) SendRaw(frame []byte) ([][]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return nil, fmt.Errorf("midea: nicht verbunden")
	}
	return a.conn.sendCommand(a.DeviceID, frame, DefaultTimeout)
}
