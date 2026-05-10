package proto

import "fmt"

// TLV types in discovery response payloads (saison 7).
// All values from CreateDeviceFromDiscoveryMessage disassembly and
// live capture verification.

const (
	TLVTypeMAC           uint8 = 0x21 // device MAC, hex string no colons
	TLVTypeMACIP         uint8 = 0x02 // MAC + IP combined
	TLVTypeName          uint8 = 0x0b // "UA Intercom Viewer 4242"
	TLVTypeFirmware      uint8 = 0x03 // "DA.qca805x.v1.5.30..."
	TLVTypeModel         uint8 = 0x15 // "UA-Int-Viewer"
	TLVTypeServicePort   uint8 = 0x24 // 8080, big-endian 32-bit
	TLVTypeAppVersion    uint8 = 0x16 // "v1.0"
	TLVTypeGUID          uint8 = 0x2b // 16-byte UUID
	TLVTypeAdopted       uint8 = 0x17 // single byte, INVERSE bool
	TLVTypeDiscriminator uint8 = 0x82 // AP (0x01) vs normal device
)

// TLV represents one type-length-value triple from a discovery message.
type TLV struct {
	Type   uint8
	Length uint16
	Value  []byte
}

// EncodeTLVs serializes TLVs into the wire format used by devices.
// Format per TLV: type(1B) + length(2B big-endian) + value(N bytes).
// The Length field on the input struct is ignored; len(Value) is used.
func EncodeTLVs(tlvs []TLV) []byte {
	size := 0
	for _, t := range tlvs {
		size += 3 + len(t.Value)
	}
	out := make([]byte, 0, size)
	for _, t := range tlvs {
		l := uint16(len(t.Value))
		out = append(out, t.Type, byte(l>>8), byte(l))
		out = append(out, t.Value...)
	}
	return out
}

// DecodeTLVs parses TLVs from a raw discovery response payload.
// Returns error if length fields overrun the buffer.
func DecodeTLVs(data []byte) ([]TLV, error) {
	var tlvs []TLV
	for i := 0; i < len(data); {
		if i+3 > len(data) {
			return nil, fmt.Errorf("proto: truncated TLV header at offset %d", i)
		}
		t := data[i]
		l := uint16(data[i+1])<<8 | uint16(data[i+2])
		i += 3
		if i+int(l) > len(data) {
			return nil, fmt.Errorf("proto: TLV value at offset %d claims %d bytes, only %d remain", i-3, l, len(data)-i)
		}
		v := make([]byte, l)
		copy(v, data[i:i+int(l)])
		tlvs = append(tlvs, TLV{Type: t, Length: l, Value: v})
		i += int(l)
	}
	return tlvs, nil
}
