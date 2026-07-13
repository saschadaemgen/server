// Pairing und Credential-Beschaffung fuer V3-Geraete.
//
// Hintergrund (wichtig, siehe COVERAGE.md): Die Zielgeraete sprechen Midea-
// Protokoll V3. Token und Key sind bei V3 NICHT lokal auslesbar - sie werden
// bei der Ersteinrichtung zwischen Geraet und Midea-Cloud ausgehandelt und sind
// nur ueber das Cloud-Konto des Nutzers abrufbar. Die LAN-Discovery (welche
// Geraete, IP, Device-ID, Protokoll-Version) laeuft rein lokal; die CREDENTIALS
// fuer V3 nicht.
//
// Ablauf der Adoption:
//  1. DiscoverLocal    - rein lokal: findet Geraete im Netz (UDP).
//  2. Credential-Beschaffung, zwei Wege:
//     a) CloudRetrieve    - einmalige, provisionierte Anmeldung am Midea-Konto,
//     holt Token+Key pro Geraet (zu portierender Block).
//     b) ImportCredentials- bereits beschaffte Token/Key (z. B. einmalig extern
//     gezogen) verschluesselt uebernehmen (Fallback).
//  3. VerifyLocal      - rein lokal: prueft Token+Key per 8370-Handshake.
//  4. Persist          - verschluesselt ablegen (macht CARVILON-Provisioning).
//
// Danach laeuft der Betrieb vollstaendig lokal.
package mideaclimate

import (
	"context"
	"fmt"
	"time"

	"carvilon.local/server/internal/midea"
)

// Discovered ist ein lokal gefundenes Geraet (noch ohne Credentials).
type Discovered struct {
	IP         string
	DeviceID   uint64
	Name       string
	ProtocolV3 bool // true = Token/Key noetig (Cloud), false = V2 (lokal moeglich)
}

// DiscoverLocal findet Geraete rein lokal im Netz (UDP-Broadcast/Unicast).
// Kein Cloud-Zugriff. host optional: leer = Broadcast, gesetzt = gezielt (robust
// unter Windows/ueber VLAN-Grenzen).
func DiscoverLocal(ctx context.Context, host string, timeout time.Duration) ([]Discovered, error) {
	var devs []midea.DiscoveredDevice
	var err error
	if host != "" {
		devs, err = midea.DiscoverHost(host, timeout)
	} else {
		devs, err = midea.Discover(timeout)
	}
	if err != nil {
		return nil, err
	}
	out := make([]Discovered, 0, len(devs))
	for _, d := range devs {
		out = append(out, Discovered{
			IP: d.IP, DeviceID: d.DeviceID, Name: d.Name,
			ProtocolV3: d.Version >= 3,
		})
	}
	return out, nil
}

// CredentialSource ist die Abstraktion der Beschaffung. Zwei Implementierungen:
// cloudRetriever (cloud.go) und importedCredentials.
type CredentialSource interface {
	// Fetch liefert Token+Key fuer ein lokal entdecktes Geraet.
	Fetch(ctx context.Context, dev Discovered) (token, key []byte, err error)
}

// --- Weg (b): Import bereits beschaffter Credentials (Fallback) --------------

type importedCredentials struct {
	byDeviceID map[uint64]Credentials
}

// NewImportedCredentials uebernimmt extern beschaffte Token/Key (verschluesselt
// bereitgestellt). Robuster Fallback, falls die Cloud-API klemmt oder von Midea
// abgeschaltet wird.
func NewImportedCredentials(creds []Credentials) CredentialSource {
	m := make(map[uint64]Credentials, len(creds))
	for _, c := range creds {
		m[c.DeviceID] = c
	}
	return &importedCredentials{byDeviceID: m}
}

func (i *importedCredentials) Fetch(_ context.Context, dev Discovered) ([]byte, []byte, error) {
	c, ok := i.byDeviceID[dev.DeviceID]
	if !ok {
		return nil, nil, fmt.Errorf("keine importierten Credentials fuer Geraet %d", dev.DeviceID)
	}
	return c.Token, c.Key, nil
}

// --- Weg (a): Cloud-Abruf ueber die NetHome-Plus-Cloud ----------------------
//
// Implementiert in cloud.go (NewCloudRetriever). Einmaliger Abruf pro neuem
// Geraet mit generischen App-Zugangsdaten (kein Nutzer-Konto noetig); danach
// laeuft alles lokal. Siehe cloud.go fuer Details und die Export-Funktion, mit
// der der Nutzer seine unbefristeten V3-Schluessel sichtbar sichern kann.

// --- Pairing-Ablauf ----------------------------------------------------------

// Pair fuehrt die Adoption durch: lokale Verifikation der (via src beschafften)
// Credentials per 8370-Handshake. Liefert eine betriebsbereite Credentials-
// Struktur, die CARVILON verschluesselt ablegt.
func Pair(ctx context.Context, dev Discovered, src CredentialSource) (Credentials, error) {
	token, key, err := src.Fetch(ctx, dev)
	if err != nil {
		return Credentials{}, err
	}
	creds := Credentials{IP: dev.IP, DeviceID: dev.DeviceID, Token: token, Key: key}

	// VerifyLocal: rein lokaler Handshake-Test, dass Token+Key stimmen.
	d, err := Provision(ctx, dev.IP, creds)
	if err != nil {
		return Credentials{}, fmt.Errorf("verifikation fehlgeschlagen: %w", err)
	}
	_ = d.Deprovision(ctx)
	return creds, nil
}
