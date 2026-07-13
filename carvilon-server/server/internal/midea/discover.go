package midea

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// discoveryMsg ist das feste Broadcast-Paket, mit dem Midea-Geräte zur Antwort
// aufgefordert werden.
var discoveryMsg = mustHex(
	"5a5a01114800920000000000000000000000000000000000000000000000000000000000000000007f75bd6b3e4f8b762e849c6e578d6590036e9d4342a50f1f569eb8ec918e92e5")

// discoveryPorts sind die UDP-Ports, an die das Broadcast-Paket geht.
var discoveryPorts = []int{6445, 20086}

// DiscoveredDevice beschreibt ein im Netz gefundenes Gerät.
// Token/Key sind NICHT enthalten – die stammen aus der Cloud bzw. sind bei dir
// bereits gespeichert. Discovery liefert Adresse, ID, Typ und Version.
type DiscoveredDevice struct {
	IP         string
	Port       int
	DeviceID   uint64
	Name       string // z. B. "net_ac_7062"
	SN         string // Seriennummer
	DeviceType byte   // 0xAC = Klimagerät
	Version    int    // 2 oder 3
}

// SupportedAC meldet, ob es sich um ein steuerbares Klimagerät handelt.
func (d DiscoveredDevice) SupportedAC() bool { return d.DeviceType == deviceTypeAC }

// Discover sendet einen Broadcast über alle Interfaces und sammelt Antworten
// bis zum Timeout ein.
func Discover(timeout time.Duration) ([]DiscoveredDevice, error) {
	return discover("255.255.255.255", timeout, true)
}

// DiscoverHost fragt gezielt einen einzelnen Host per Unicast ab (robuster als
// Broadcast, u. a. unter Windows/über VLAN-Grenzen hinweg).
func DiscoverHost(host string, timeout time.Duration) ([]DiscoveredDevice, error) {
	return discover(host, timeout, false)
}

func discover(target string, timeout time.Duration, broadcast bool) ([]DiscoveredDevice, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("midea: UDP-Socket: %w", err)
	}
	defer conn.Close()

	// Broadcast senden bzw. gezielt an den Host.
	for _, port := range discoveryPorts {
		addr := &net.UDPAddr{IP: net.ParseIP(target), Port: port}
		if addr.IP == nil {
			// Hostname statt IP: auflösen.
			ips, resErr := net.LookupIP(target)
			if resErr != nil || len(ips) == 0 {
				return nil, fmt.Errorf("midea: Host %q nicht auflösbar", target)
			}
			addr.IP = ips[0]
		}
		for i := 0; i < 3; i++ { // 3 Pakete gegen UDP-Verlust
			_, _ = conn.WriteToUDP(discoveryMsg, addr)
		}
	}

	found := make(map[string]DiscoveredDevice)
	deadline := time.Now().Add(timeout)
	_ = conn.SetReadDeadline(deadline)

	buf := make([]byte, 512)
	for time.Now().Before(deadline) {
		n, remote, rerr := conn.ReadFromUDP(buf)
		if rerr != nil {
			break // Timeout erreicht
		}
		if _, seen := found[remote.IP.String()]; seen {
			continue
		}
		dev, perr := parseDiscoveryResponse(remote.IP.String(), buf[:n])
		if perr != nil {
			continue // fremdes/unlesbares Paket ignorieren
		}
		found[remote.IP.String()] = dev
	}

	out := make([]DiscoveredDevice, 0, len(found))
	for _, d := range found {
		out = append(out, d)
	}
	return out, nil
}

// deviceVersion erkennt die Protokollversion anhand der Startbytes.
func deviceVersion(data []byte) (int, error) {
	if len(data) < 2 {
		return 0, errors.New("midea: Antwort zu kurz")
	}
	switch {
	case data[0] == 0x5A && data[1] == 0x5A:
		return 2, nil
	case data[0] == 0x83 && data[1] == 0x70:
		return 3, nil
	default:
		// V1 wäre XML – von uns nicht unterstützt.
		return 0, errors.New("midea: unbekannte Geräteversion")
	}
}

// parseDiscoveryResponse entschlüsselt und zerlegt eine Discovery-Antwort.
func parseDiscoveryResponse(ip string, raw []byte) (DiscoveredDevice, error) {
	version, err := deviceVersion(raw)
	if err != nil {
		return DiscoveredDevice{}, err
	}

	data := raw
	if version == 3 {
		// V3: 8-Byte-Header und 16-Byte-Hash abstreifen → innerer 5A5A-Block.
		if len(raw) < 24 {
			return DiscoveredDevice{}, errors.New("midea: V3-Antwort zu kurz")
		}
		data = raw[8 : len(raw)-16]
	}
	if len(data) < 56 {
		return DiscoveredDevice{}, errors.New("midea: Antwort zu kurz für Parsing")
	}

	encrypted := data[40 : len(data)-16]
	deviceID := uint64(binary.LittleEndian.Uint32(data[20:24])) |
		uint64(data[24])<<32 | uint64(data[25])<<40 // 6-Byte-LE-ID

	dec, err := decryptAESECB(encrypted)
	if err != nil {
		return DiscoveredDevice{}, fmt.Errorf("midea: Discovery-Entschlüsselung: %w", err)
	}
	if len(dec) < 41 {
		return DiscoveredDevice{}, errors.New("midea: entschlüsselte Antwort zu kurz")
	}

	// IP = erste 4 Bytes in umgekehrter Reihenfolge.
	repIP := net.IPv4(dec[3], dec[2], dec[1], dec[0]).String()
	port := int(binary.LittleEndian.Uint16(dec[4:6]))
	sn := strings.TrimRight(string(dec[8:40]), "\x00")

	nameLen := int(dec[40])
	if 41+nameLen > len(dec) {
		return DiscoveredDevice{}, errors.New("midea: Name-Länge außerhalb des Puffers")
	}
	name := string(dec[41 : 41+nameLen])

	// Gerätetyp steckt im Namen: "net_<typ>_<suffix>", z. B. "net_ac_7062".
	var devType byte
	if parts := strings.Split(name, "_"); len(parts) >= 2 {
		if t, perr := strconv.ParseUint(parts[1], 16, 8); perr == nil {
			devType = byte(t)
		}
	}

	// Gemeldete IP nur informativ; wir nutzen die tatsächlich empfangene.
	_ = repIP

	return DiscoveredDevice{
		IP:         ip,
		Port:       port,
		DeviceID:   deviceID,
		Name:       name,
		SN:         sn,
		DeviceType: devType,
		Version:    version,
	}, nil
}

// mustHex dekodiert einen Hex-String oder panict (nur für Konstanten).
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
