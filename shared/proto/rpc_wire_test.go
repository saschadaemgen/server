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
