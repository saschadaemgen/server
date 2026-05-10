package discovery

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"unifix.local/mock/internal/identity"
	"unifix.local/shared/proto"
)

// freshUptime is the placeholder uptime value the saison 9 mock
// reports. Small enough that UDM treats the device as freshly
// powered on.
const freshUptime uint64 = 65

// buildResponse constructs the complete 17-TLV discovery response
// packet matching the saison 9 pcap-verified format (viewer-reset-
// 100518.pcap, frame 34). The returned bytes start with the 4-byte
// packet header (0x01 0x00 + uint16 BE payload length) followed by
// the TLV payload in fixed order.
func buildResponse(id *identity.MockIdentity) ([]byte, error) {
	nowSec := time.Now().Unix()

	macHex := strings.ToLower(strings.ReplaceAll(id.MAC.String(), ":", ""))

	ipv4 := id.IPv4.To4()
	if ipv4 == nil {
		return nil, fmt.Errorf("identity ipv4 is not IPv4: %v", id.IPv4)
	}
	macIP := make([]byte, 0, 10)
	macIP = append(macIP, []byte(id.MAC)...)
	macIP = append(macIP, ipv4...)

	portBE := make([]byte, 4)
	binary.BigEndian.PutUint32(portBE, uint32(id.ServicePort))

	ts1 := make([]byte, 8)
	binary.BigEndian.PutUint32(ts1[4:], uint32(nowSec))

	uptime := make([]byte, 8)
	binary.BigEndian.PutUint64(uptime, freshUptime)

	guidBytes, err := parseGUIDBytes(id.GUID)
	if err != nil {
		return nil, fmt.Errorf("identity guid invalid: %w", err)
	}

	ts2 := make([]byte, 8)
	binary.BigEndian.PutUint32(ts2[4:], uint32(nowSec-60))

	tlvs := []proto.TLV{
		{Type: proto.TLVTypeMAC, Value: []byte(macHex)},
		{Type: proto.TLVTypeMACIP, Value: macIP},
		{Type: proto.TLVTypeFirmware, Value: []byte(id.Firmware)},
		{Type: proto.TLVTypeServicePort, Value: portBE},
		{Type: proto.TLVTypeTimestamp1, Value: ts1},
		{Type: proto.TLVTypeEmpty28, Value: []byte{}},
		{Type: proto.TLVTypeUser, Value: []byte("user")},
		{Type: proto.TLVTypeUptime, Value: uptime},
		{Type: proto.TLVTypeGUID, Value: guidBytes},
		{Type: proto.TLVTypeName, Value: []byte(id.Name)},
		{Type: proto.TLVTypeOnes, Value: []byte{0x01}},
		{Type: proto.TLVTypeTimestamp2, Value: ts2},
		{Type: proto.TLVTypeHWType, Value: []byte(id.HWType)},
		{Type: proto.TLVTypeBOMRev, Value: []byte(id.BOMRev)},
		{Type: proto.TLVTypeModel, Value: []byte(id.Model)},
		{Type: proto.TLVTypeAppVersion, Value: []byte(id.AppVersion)},
		{Type: proto.TLVTypeReady, Value: []byte{0x01}},
	}

	payload := proto.EncodeTLVs(tlvs)

	out := make([]byte, 4+len(payload))
	out[0] = 0x01
	out[1] = 0x00
	binary.BigEndian.PutUint16(out[2:4], uint16(len(payload)))
	copy(out[4:], payload)
	return out, nil
}

// isDiscoveryProbe returns true if payload matches the 4-byte UDM
// discovery probe magic.
func isDiscoveryProbe(payload []byte) bool {
	return bytes.Equal(payload, proto.DiscoveryProbeMagic)
}

// parseGUIDBytes converts a canonical UUID string (with or without
// dashes) into its 16 raw bytes.
func parseGUIDBytes(guid string) ([]byte, error) {
	cleaned := strings.ReplaceAll(guid, "-", "")
	if len(cleaned) != 32 {
		return nil, fmt.Errorf("guid must be 32 hex chars, got %d", len(cleaned))
	}
	return hex.DecodeString(cleaned)
}
