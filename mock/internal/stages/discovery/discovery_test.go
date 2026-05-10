package discovery

import (
	"encoding/binary"
	"net"
	"testing"

	"unifix.local/mock/internal/identity"
	"unifix.local/shared/proto"
)

const testGUID = "2f840033-e0ce-4cf0-971a-25e61c275d07"

func mustIdentity(t *testing.T, mac, ipv4, name, guid string, port uint16) *identity.MockIdentity {
	t.Helper()
	hw, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("parse mac: %v", err)
	}
	ip := net.ParseIP(ipv4).To4()
	if ip == nil {
		t.Fatalf("parse ip: %s", ipv4)
	}
	id, err := identity.NewMockIdentity(hw, name, guid, ip, port)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return id
}

func mustResponse(t *testing.T, id *identity.MockIdentity) []byte {
	t.Helper()
	out, err := buildResponse(id)
	if err != nil {
		t.Fatalf("buildResponse: %v", err)
	}
	return out
}

func decodePayload(t *testing.T, packet []byte) []proto.TLV {
	t.Helper()
	if len(packet) < 4 {
		t.Fatalf("packet too short: %d bytes", len(packet))
	}
	tlvs, err := proto.DecodeTLVs(packet[4:])
	if err != nil {
		t.Fatalf("DecodeTLVs: %v", err)
	}
	return tlvs
}

func findTLV(t *testing.T, tlvs []proto.TLV, typ uint8) proto.TLV {
	t.Helper()
	for _, tl := range tlvs {
		if tl.Type == typ {
			return tl
		}
	}
	t.Fatalf("TLV 0x%02x not found", typ)
	return proto.TLV{}
}

func TestBuildResponse_HasPacketHeader(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	pkt := mustResponse(t, id)
	if len(pkt) < 4 {
		t.Fatalf("packet too short: %d", len(pkt))
	}
	if pkt[0] != 0x01 || pkt[1] != 0x00 {
		t.Errorf("header magic = % x, want 01 00", pkt[:2])
	}
	declaredLen := binary.BigEndian.Uint16(pkt[2:4])
	actualLen := uint16(len(pkt) - 4)
	if declaredLen != actualLen {
		t.Errorf("header payload length = %d, want %d", declaredLen, actualLen)
	}
}

func TestBuildResponse_ContainsAll17TLVs(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	if len(tlvs) != 17 {
		t.Fatalf("got %d TLVs, want 17", len(tlvs))
	}
	wantOrder := []uint8{
		0x21, 0x02, 0x03, 0x24, 0x27, 0x28, 0x2a, 0x0a, 0x2b,
		0x0b, 0x2c, 0x2d, 0x2e, 0x2f, 0x15, 0x16, 0x17,
	}
	for i, want := range wantOrder {
		if tlvs[i].Type != want {
			t.Errorf("tlv[%d].Type = 0x%02x, want 0x%02x", i, tlvs[i].Type, want)
		}
	}
}

func TestBuildResponse_TLV21IsUTF8MACHex(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeMAC)
	if got := string(tlv.Value); got != "0cea14424242" {
		t.Errorf("TLV 0x21 = %q, want %q", got, "0cea14424242")
	}
	if len(tlv.Value) != 12 {
		t.Errorf("TLV 0x21 length = %d, want 12", len(tlv.Value))
	}
}

func TestBuildResponse_TLV02IsMACPlusIP(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeMACIP)
	want := []byte{0x0c, 0xea, 0x14, 0x42, 0x42, 0x42, 0xc0, 0xa8, 0x01, 0x2a}
	if len(tlv.Value) != len(want) {
		t.Fatalf("TLV 0x02 length = %d, want %d", len(tlv.Value), len(want))
	}
	for i := range want {
		if tlv.Value[i] != want[i] {
			t.Errorf("TLV 0x02 = % x, want % x", tlv.Value, want)
			break
		}
	}
}

func TestBuildResponse_TLV17IsReady(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeReady)
	if len(tlv.Value) != 1 || tlv.Value[0] != 0x01 {
		t.Errorf("TLV 0x17 = % x, want 01 (ready=true, saison-9 pcap)", tlv.Value)
	}
}

func TestBuildResponse_TLV2eIsHWType(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeHWType)
	if got := string(tlv.Value); got != "GA" {
		t.Errorf("TLV 0x2e = %q, want %q", got, "GA")
	}
}

func TestBuildResponse_TLV2fIsBOMRev(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeBOMRev)
	if got := string(tlv.Value); got != "05" {
		t.Errorf("TLV 0x2f = %q, want %q", got, "05")
	}
}

func TestBuildResponse_TLV0aIsUptime8Bytes(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeUptime)
	if len(tlv.Value) != 8 {
		t.Errorf("TLV 0x0a length = %d, want 8", len(tlv.Value))
	}
}

func TestBuildResponse_TLV24IsServicePortBE(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeServicePort)
	want := []byte{0x00, 0x00, 0x1f, 0x90}
	if len(tlv.Value) != 4 {
		t.Fatalf("TLV 0x24 length = %d, want 4", len(tlv.Value))
	}
	for i := range want {
		if tlv.Value[i] != want[i] {
			t.Errorf("TLV 0x24 = % x, want % x", tlv.Value, want)
			break
		}
	}
}

func TestBuildResponse_TLV2bIsGUIDRawBytes(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeGUID)
	if int(tlv.Length) != 16 {
		t.Fatalf("TLV 0x2b length = %d, want 16", tlv.Length)
	}
	want := []byte{0x2f, 0x84, 0x00, 0x33}
	for i := range want {
		if tlv.Value[i] != want[i] {
			t.Errorf("TLV 0x2b first 4 bytes = % x, want % x", tlv.Value[:4], want)
			break
		}
	}
}

func TestBuildResponse_TLV2aIsUser(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeUser)
	if got := string(tlv.Value); got != "user" {
		t.Errorf("TLV 0x2a = %q, want %q", got, "user")
	}
}

func TestBuildResponse_TLV2cIsSingleOne(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeOnes)
	if len(tlv.Value) != 1 || tlv.Value[0] != 0x01 {
		t.Errorf("TLV 0x2c = % x, want 01", tlv.Value)
	}
}

func TestBuildResponse_TLV28IsEmpty(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	tlv := findTLV(t, tlvs, proto.TLVTypeEmpty28)
	if len(tlv.Value) != 0 {
		t.Errorf("TLV 0x28 length = %d, want 0", len(tlv.Value))
	}
}

func TestBuildResponse_TLV27AndTLV2dTimestamps(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	ts1 := findTLV(t, tlvs, proto.TLVTypeTimestamp1)
	ts2 := findTLV(t, tlvs, proto.TLVTypeTimestamp2)
	if len(ts1.Value) != 8 {
		t.Errorf("TLV 0x27 length = %d, want 8", len(ts1.Value))
	}
	if len(ts2.Value) != 8 {
		t.Errorf("TLV 0x2d length = %d, want 8", len(ts2.Value))
	}
	// First 4 bytes of each timestamp TLV are zero per saison 9.
	for i := 0; i < 4; i++ {
		if ts1.Value[i] != 0 {
			t.Errorf("TLV 0x27 byte %d = 0x%02x, want 0x00", i, ts1.Value[i])
		}
		if ts2.Value[i] != 0 {
			t.Errorf("TLV 0x2d byte %d = 0x%02x, want 0x00", i, ts2.Value[i])
		}
	}
	// ts2 == ts1 - 60 seconds.
	ts1Sec := binary.BigEndian.Uint32(ts1.Value[4:])
	ts2Sec := binary.BigEndian.Uint32(ts2.Value[4:])
	if ts1Sec-ts2Sec != 60 {
		t.Errorf("ts1 - ts2 = %d, want 60", ts1Sec-ts2Sec)
	}
}

func TestBuildResponse_TLVNameAndModelAndFirmware(t *testing.T) {
	id := mustIdentity(t, "0c:ea:14:42:42:42", "192.168.1.42", "", testGUID, 8080)
	tlvs := decodePayload(t, mustResponse(t, id))
	if got := string(findTLV(t, tlvs, proto.TLVTypeName).Value); got != "UA Intercom Viewer 4242" {
		t.Errorf("Name = %q, want %q", got, "UA Intercom Viewer 4242")
	}
	if got := string(findTLV(t, tlvs, proto.TLVTypeModel).Value); got != "UA-Int-Viewer" {
		t.Errorf("Model = %q, want %q", got, "UA-Int-Viewer")
	}
	if got := string(findTLV(t, tlvs, proto.TLVTypeAppVersion).Value); got != "v1.0" {
		t.Errorf("AppVersion = %q, want %q", got, "v1.0")
	}
	if got := string(findTLV(t, tlvs, proto.TLVTypeFirmware).Value); got != identity.DefaultFirmware {
		t.Errorf("Firmware = %q, want %q", got, identity.DefaultFirmware)
	}
}

func TestIsDiscoveryProbe_Magic(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"exact-magic", []byte{0x01, 0x00, 0x00, 0x00}, true},
		{"wrong-last-byte", []byte{0x01, 0x00, 0x00, 0x01}, false},
		{"empty", []byte{}, false},
		{"too-short", []byte{0x01, 0x00, 0x00}, false},
		{"too-long", []byte{0x01, 0x00, 0x00, 0x00, 0x00}, false},
		{"all-zero", []byte{0x00, 0x00, 0x00, 0x00}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDiscoveryProbe(c.payload); got != c.want {
				t.Errorf("isDiscoveryProbe(% x) = %v, want %v", c.payload, got, c.want)
			}
		})
	}
}
