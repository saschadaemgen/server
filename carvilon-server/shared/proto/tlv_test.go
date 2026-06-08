package proto

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestTLVRoundtrip(t *testing.T) {
	portBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(portBuf, 8080)

	in := []TLV{
		{Type: TLVTypeName, Value: []byte("UA Intercom Viewer 4242")},
		{Type: TLVTypeMAC, Value: []byte("0cea1442 4242")},
		{Type: TLVTypeServicePort, Value: portBuf},
		{Type: TLVTypeReady, Value: []byte{0x01}},
	}

	encoded := EncodeTLVs(in)

	out, err := DecodeTLVs(encoded)
	if err != nil {
		t.Fatalf("DecodeTLVs: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d TLVs, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Type != in[i].Type {
			t.Errorf("tlv[%d].Type = 0x%02x, want 0x%02x", i, out[i].Type, in[i].Type)
		}
		if int(out[i].Length) != len(in[i].Value) {
			t.Errorf("tlv[%d].Length = %d, want %d", i, out[i].Length, len(in[i].Value))
		}
		if !bytes.Equal(out[i].Value, in[i].Value) {
			t.Errorf("tlv[%d].Value = %q, want %q", i, out[i].Value, in[i].Value)
		}
	}
}

func TestTLVEncodeWireFormat(t *testing.T) {
	in := []TLV{
		{Type: TLVTypeModel, Value: []byte("UA-Int-Viewer")},
	}
	got := EncodeTLVs(in)
	want := append([]byte{TLVTypeModel, 0x00, 0x0d}, []byte("UA-Int-Viewer")...)
	if !bytes.Equal(got, want) {
		t.Errorf("encoded = % x, want % x", got, want)
	}
}

func TestTLVEmptyValue(t *testing.T) {
	in := []TLV{{Type: TLVTypeAppVersion, Value: []byte{}}}
	encoded := EncodeTLVs(in)
	want := []byte{TLVTypeAppVersion, 0x00, 0x00}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("encoded = % x, want % x", encoded, want)
	}
	out, err := DecodeTLVs(encoded)
	if err != nil {
		t.Fatalf("DecodeTLVs: %v", err)
	}
	if len(out) != 1 || out[0].Type != TLVTypeAppVersion || len(out[0].Value) != 0 {
		t.Fatalf("got %+v, want one empty-value TLV", out)
	}
}

func TestTLVLengthOverflow(t *testing.T) {
	// Type 0x21, claimed length 0x0010 (16), but only 5 bytes follow.
	bad := []byte{0x21, 0x00, 0x10, 'a', 'b', 'c', 'd', 'e'}
	if _, err := DecodeTLVs(bad); err == nil {
		t.Fatal("DecodeTLVs accepted length-overflow buffer; want error")
	}
}

func TestTLVTruncatedHeader(t *testing.T) {
	bad := []byte{0x21, 0x00} // missing third header byte
	if _, err := DecodeTLVs(bad); err == nil {
		t.Fatal("DecodeTLVs accepted truncated header; want error")
	}
}

func TestTLVEmptyInput(t *testing.T) {
	out, err := DecodeTLVs(nil)
	if err != nil {
		t.Fatalf("DecodeTLVs(nil): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("len = %d, want 0", len(out))
	}
}
