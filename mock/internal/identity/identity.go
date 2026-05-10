// Package identity holds the per-mock fixed metadata that the mock
// daemon announces in discovery responses and uses in adoption
// payloads.
package identity

import (
	"crypto/rand"
	"fmt"
	"net"
	"strings"
)

const (
	DefaultModel      = "UA-Int-Viewer"
	DefaultAppVersion = "v1.0"
	DefaultFirmware   = "DA.qca805x.v1.5.30.000000.19700121.113317"
)

// MockIdentity captures the wer-bin-ich-Karte of one mock device.
// Constructed once at startup from CLI flags, then immutable.
type MockIdentity struct {
	MAC         net.HardwareAddr
	ID          string // MAC hex without colons
	Name        string
	IPv4        net.IP
	GUID        string // canonical 8-4-4-4-12 UUID string
	ServicePort uint16
	Model       string
	AppVersion  string
	Firmware    string
}

// NewMockIdentity validates inputs, derives ID from the MAC, fills
// in defaults for the optional fields, and returns the immutable
// identity record.
func NewMockIdentity(
	mac net.HardwareAddr,
	name, guid string,
	ipv4 net.IP,
	servicePort uint16,
) (*MockIdentity, error) {
	if len(mac) != 6 {
		return nil, fmt.Errorf("identity: MAC must be 6 bytes, got %d", len(mac))
	}
	if ipv4 == nil || ipv4.To4() == nil {
		return nil, fmt.Errorf("identity: ipv4 must be a valid IPv4 address, got %v", ipv4)
	}
	if servicePort == 0 {
		return nil, fmt.Errorf("identity: servicePort must be > 0")
	}

	if guid == "" {
		generated, err := newUUIDv4()
		if err != nil {
			return nil, fmt.Errorf("identity: generate guid: %w", err)
		}
		guid = generated
	} else if !isValidUUID(guid) {
		return nil, fmt.Errorf("identity: guid is not a valid UUID: %q", guid)
	}

	if name == "" {
		name = defaultName(mac)
	}

	return &MockIdentity{
		MAC:         mac,
		ID:          macToID(mac),
		Name:        name,
		IPv4:        ipv4.To4(),
		GUID:        guid,
		ServicePort: servicePort,
		Model:       DefaultModel,
		AppVersion:  DefaultAppVersion,
		Firmware:    DefaultFirmware,
	}, nil
}

// String returns a compact human readable summary, useful for logs.
func (i *MockIdentity) String() string {
	return fmt.Sprintf("MockIdentity{MAC=%s ID=%s Name=%q IP=%s Port=%d GUID=%s Model=%s}",
		i.MAC.String(), i.ID, i.Name, i.IPv4.String(), i.ServicePort, i.GUID, i.Model)
}

// macToID returns the lowercase hex form of the MAC without colons.
func macToID(mac net.HardwareAddr) string {
	return strings.ReplaceAll(strings.ToLower(mac.String()), ":", "")
}

// defaultName follows the saison 7 schema:
// "UA Intercom Viewer " + last 4 hex chars of the MAC.
func defaultName(mac net.HardwareAddr) string {
	return fmt.Sprintf("UA Intercom Viewer %02x%02x", mac[4], mac[5])
}

// isValidUUID checks for the canonical 8-4-4-4-12 lowercase or
// uppercase hex form. Does not enforce a specific version or variant.
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexDigit(c) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'f') ||
		(c >= 'A' && c <= 'F')
}

// newUUIDv4 produces a fresh RFC-4122 version-4 UUID string using
// crypto/rand. Sets version (0x40) and variant (0x80) bits.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
