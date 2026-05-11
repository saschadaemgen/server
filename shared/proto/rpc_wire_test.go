package proto

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRPCRoundtrip(t *testing.T) {
	encoded := EncodeRPCResponse(RPCMethodRemoteView, "req-42", "success")

	req, err := DecodeRPCRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeRPCRequest: %v", err)
	}
	if req.Path != RPCMethodRemoteView {
		t.Errorf("Path = %q, want %q", req.Path, RPCMethodRemoteView)
	}
	if req.RequestID != "req-42" {
		t.Errorf("RequestID = %q, want %q", req.RequestID, "req-42")
	}
	if !bytes.Equal(req.Raw, encoded) {
		t.Errorf("Raw mismatch: % x vs % x", req.Raw, encoded)
	}
}

func TestRPCRoundtripEmptyStatus(t *testing.T) {
	encoded := EncodeRPCResponse(RPCMethodUpdateConfigs, "id-1", "")
	req, err := DecodeRPCRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeRPCRequest: %v", err)
	}
	if req.Path != RPCMethodUpdateConfigs {
		t.Errorf("Path = %q, want %q", req.Path, RPCMethodUpdateConfigs)
	}
	if req.RequestID != "id-1" {
		t.Errorf("RequestID = %q, want %q", req.RequestID, "id-1")
	}
}

func TestRPCEncodeWireShape(t *testing.T) {
	encoded := EncodeRPCResponse("/x", "y", "")
	if len(encoded) == 0 {
		t.Fatal("encoded is empty")
	}
	if encoded[0] != 0x12 {
		t.Errorf("outer tag = 0x%02x, want 0x12", encoded[0])
	}
	// After outer wrapper there should be a body whose first byte is
	// 0x0a (a map entry).
	if len(encoded) < 3 || encoded[2] != 0x0a {
		t.Errorf("expected first body byte = 0x0a (map entry), got % x", encoded[:min(8, len(encoded))])
	}
}

func TestRPCDecodeRejectsEmpty(t *testing.T) {
	if _, err := DecodeRPCRequest(nil); err == nil {
		t.Fatal("DecodeRPCRequest(nil) accepted; want error")
	}
}

func TestRPCDecodeRejectsTruncatedOuter(t *testing.T) {
	// Outer tag plus a length claiming 99 bytes that aren't there.
	bad := []byte{0x12, 0x63}
	if _, err := DecodeRPCRequest(bad); err == nil {
		t.Fatal("DecodeRPCRequest with truncated outer accepted; want error")
	}
}

func TestDecodeRPCRequest_WithOuterWrapper_LegacyFormat(t *testing.T) {
	encoded := EncodeRPCResponse(RPCMethodUpdateTokens, "id-42", "")
	if encoded[0] != 0x12 {
		t.Fatalf("test fixture should start with 0x12 outer tag, got 0x%02x", encoded[0])
	}
	req, err := DecodeRPCRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeRPCRequest legacy: %v", err)
	}
	if req.Path != RPCMethodUpdateTokens {
		t.Errorf("Path = %q, want %q", req.Path, RPCMethodUpdateTokens)
	}
	if req.RequestID != "id-42" {
		t.Errorf("RequestID = %q, want %q", req.RequestID, "id-42")
	}
}

func TestDecodeRPCRequest_WithoutOuterWrapper_UDMNativeFormat(t *testing.T) {
	// Real bytes captured saison 10 from UDM's /update_tokens RPC
	// to our mock. Frame starts directly with 0x0a (no 0x12 outer).
	data, err := os.ReadFile(filepath.Join("testdata", "saison10_update_tokens.bin"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	if data[0] != 0x0a {
		t.Fatalf("test fixture should start with 0x0a, got 0x%02x", data[0])
	}
	req, err := DecodeRPCRequest(data)
	if err != nil {
		t.Fatalf("DecodeRPCRequest UDM-native: %v", err)
	}
	if req.Path != RPCMethodUpdateTokens {
		t.Errorf("Path = %q, want %q", req.Path, RPCMethodUpdateTokens)
	}
	if req.RequestID == "" {
		t.Error("RequestID is empty; expected a UDM-generated id")
	}
	if !bytes.Equal(req.Raw, data) {
		t.Error("Raw payload mismatch with input bytes")
	}
}

func TestDecodeRPCRequest_UnknownLeadByte(t *testing.T) {
	bad := []byte{0xff, 0x00, 0x01, 0x02}
	_, err := DecodeRPCRequest(bad)
	if err == nil {
		t.Fatal("DecodeRPCRequest with 0xff lead accepted; want error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("0xff")) {
		t.Errorf("error %q should mention the unexpected lead byte 0xff", err.Error())
	}
}

func TestDecodeRPCRequestBody_GoldmineUpdateTokens(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "saison10_update_tokens.bin"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	body, err := DecodeRPCRequestBody(data)
	if err != nil {
		t.Fatalf("DecodeRPCRequestBody: %v", err)
	}
	// Top-level map entries must be present.
	if got, _ := body["path"].(string); got != "/update_tokens" {
		t.Errorf("body[path] = %q, want %q", got, "/update_tokens")
	}
	if got, _ := body["requestId"].(string); got == "" {
		t.Errorf("body[requestId] missing or not a string")
	}
	// At least one nested config field from the bundle should be
	// flattened in. Spot-check a few known-present strings.
	knownFields := []string{
		"timezone_name",
		"live_view_timeout",
		"room_name",
		"http_cert_fingerprint",
		"intercoms",
	}
	foundAny := false
	for _, f := range knownFields {
		if _, ok := body[f]; ok {
			foundAny = true
			break
		}
	}
	if !foundAny {
		t.Errorf("expected at least one nested config field, got keys: %v", mapKeys(body))
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// protoVarintField encodes one wire-type-0 field for tests.
func protoVarintField(fieldNum uint64, value uint64) []byte {
	tag := (fieldNum << 3) | 0
	var buf [16]byte
	n := 0
	for tag >= 0x80 {
		buf[n] = byte(tag) | 0x80
		tag >>= 7
		n++
	}
	buf[n] = byte(tag)
	n++
	for value >= 0x80 {
		buf[n] = byte(value) | 0x80
		value >>= 7
		n++
	}
	buf[n] = byte(value)
	n++
	return append([]byte{}, buf[:n]...)
}

// protoStringField encodes one wire-type-2 field for tests.
func protoStringField(fieldNum uint64, value string) []byte {
	tag := (fieldNum << 3) | 2
	var out []byte
	for tag >= 0x80 {
		out = append(out, byte(tag)|0x80)
		tag >>= 7
	}
	out = append(out, byte(tag))
	length := uint64(len(value))
	for length >= 0x80 {
		out = append(out, byte(length)|0x80)
		length >>= 7
	}
	out = append(out, byte(length))
	return append(out, value...)
}

// protoFixed64Field encodes one wire-type-1 (8-byte) field used
// to exercise the skip path.
func protoFixed64Field(fieldNum uint64, payload [8]byte) []byte {
	tag := (fieldNum << 3) | 1
	var out []byte
	for tag >= 0x80 {
		out = append(out, byte(tag)|0x80)
		tag >>= 7
	}
	out = append(out, byte(tag))
	return append(out, payload[:]...)
}

func TestDecodeProtobufSubmessage_RemoteViewFields(t *testing.T) {
	var raw []byte
	raw = append(raw, protoStringField(1, "c187eb6e-323a-4994-bfea-abdfe4bdcfd3")...)
	raw = append(raw, protoVarintField(4, 82)...)
	raw = append(raw, protoStringField(5, "28704e31e29c")...)
	raw = append(raw, protoStringField(9, "WR-28704e31e29c-abc")...)

	sub, err := DecodeProtobufSubmessage(raw)
	if err != nil {
		t.Fatalf("DecodeProtobufSubmessage: %v", err)
	}
	if got, _ := sub["field_1"].(string); got != "c187eb6e-323a-4994-bfea-abdfe4bdcfd3" {
		t.Errorf("field_1 = %q, want UUID", got)
	}
	if got, _ := sub["field_4"].(int64); got != 82 {
		t.Errorf("field_4 = %v, want int64(82)", sub["field_4"])
	}
	if got, _ := sub["field_5"].(string); got != "28704e31e29c" {
		t.Errorf("field_5 = %q, want MAC", got)
	}
	if got, _ := sub["field_9"].(string); got != "WR-28704e31e29c-abc" {
		t.Errorf("field_9 = %q, want room id", got)
	}
}

func TestDecodeProtobufSubmessage_UnknownWireTypeIsSkipped(t *testing.T) {
	// Build: string field_1, fixed64 field_6 (8 bytes payload),
	// string field_5. The fixed64 must be skipped without
	// disturbing field_5 extraction.
	var raw []byte
	raw = append(raw, protoStringField(1, "first")...)
	raw = append(raw, protoFixed64Field(6, [8]byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x00, 0x00, 0x00})...)
	raw = append(raw, protoStringField(5, "fifth")...)

	sub, err := DecodeProtobufSubmessage(raw)
	if err != nil {
		t.Fatalf("DecodeProtobufSubmessage: %v (sub=%v)", err, sub)
	}
	if got, _ := sub["field_1"].(string); got != "first" {
		t.Errorf("field_1 = %q, want 'first'", got)
	}
	if got, _ := sub["field_5"].(string); got != "fifth" {
		t.Errorf("field_5 = %q, want 'fifth' (fixed64 should have been skipped)", got)
	}
}

func TestDecodeProtobufSubmessage_RepeatedFieldCoalesces(t *testing.T) {
	var raw []byte
	raw = append(raw, protoStringField(3, "first")...)
	raw = append(raw, protoStringField(3, "second")...)
	raw = append(raw, protoStringField(3, "third")...)

	sub, err := DecodeProtobufSubmessage(raw)
	if err != nil {
		t.Fatalf("DecodeProtobufSubmessage: %v", err)
	}
	list, ok := sub["field_3"].([]any)
	if !ok {
		t.Fatalf("field_3 = %T, want []any (repeated)", sub["field_3"])
	}
	if len(list) != 3 || list[0] != "first" || list[2] != "third" {
		t.Errorf("repeated field contents wrong: %v", list)
	}
}
