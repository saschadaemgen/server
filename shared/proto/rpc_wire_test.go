package proto

import (
	"bytes"
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

func TestRPCDecodeRejectsBadOuterTag(t *testing.T) {
	bad := []byte{0x0a, 0x00}
	if _, err := DecodeRPCRequest(bad); err == nil {
		t.Fatal("DecodeRPCRequest with bad outer tag accepted; want error")
	}
}

func TestRPCDecodeRejectsTruncatedOuter(t *testing.T) {
	// Outer tag plus a length claiming 99 bytes that aren't there.
	bad := []byte{0x12, 0x63}
	if _, err := DecodeRPCRequest(bad); err == nil {
		t.Fatal("DecodeRPCRequest with truncated outer accepted; want error")
	}
}
