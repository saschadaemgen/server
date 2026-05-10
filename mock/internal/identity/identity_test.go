package identity

import (
	"net"
	"strings"
	"testing"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("parse mac %q: %v", s, err)
	}
	return mac
}

func TestNewMockIdentity_ValidInputs(t *testing.T) {
	mac := mustMAC(t, "0c:ea:14:42:42:42")
	ip := net.ParseIP("192.168.1.42").To4()

	id, err := NewMockIdentity(mac, "", "", ip, 8080)
	if err != nil {
		t.Fatalf("NewMockIdentity: %v", err)
	}
	if id.ID != "0cea14424242" {
		t.Errorf("ID = %q, want %q", id.ID, "0cea14424242")
	}
	if id.Name != "UA Intercom Viewer 4242" {
		t.Errorf("Name = %q, want %q", id.Name, "UA Intercom Viewer 4242")
	}
	if id.Model != DefaultModel {
		t.Errorf("Model = %q, want %q", id.Model, DefaultModel)
	}
	if id.AppVersion != DefaultAppVersion {
		t.Errorf("AppVersion = %q, want %q", id.AppVersion, DefaultAppVersion)
	}
	if id.Firmware != DefaultFirmware {
		t.Errorf("Firmware = %q, want %q", id.Firmware, DefaultFirmware)
	}
	if !id.IPv4.Equal(net.ParseIP("192.168.1.42")) {
		t.Errorf("IPv4 = %v, want 192.168.1.42", id.IPv4)
	}
	if id.ServicePort != 8080 {
		t.Errorf("ServicePort = %d, want 8080", id.ServicePort)
	}
}

func TestNewMockIdentity_CustomName(t *testing.T) {
	mac := mustMAC(t, "0c:ea:14:42:42:42")
	id, err := NewMockIdentity(mac, "MyMock", "", net.ParseIP("192.168.1.42").To4(), 8080)
	if err != nil {
		t.Fatalf("NewMockIdentity: %v", err)
	}
	if id.Name != "MyMock" {
		t.Errorf("Name = %q, want %q", id.Name, "MyMock")
	}
}

func TestNewMockIdentity_AutoGUID(t *testing.T) {
	mac := mustMAC(t, "0c:ea:14:42:42:42")
	id, err := NewMockIdentity(mac, "", "", net.ParseIP("192.168.1.42").To4(), 8080)
	if err != nil {
		t.Fatalf("NewMockIdentity: %v", err)
	}
	if !isValidUUID(id.GUID) {
		t.Errorf("GUID = %q, not a canonical UUID", id.GUID)
	}
	// Version 4 nibble at position 14 (0-indexed in string), variant at 19.
	if id.GUID[14] != '4' {
		t.Errorf("GUID version nibble = %c, want 4", id.GUID[14])
	}
	v := id.GUID[19]
	if v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Errorf("GUID variant nibble = %c, want one of 8,9,a,b", v)
	}
}

func TestNewMockIdentity_CustomGUID(t *testing.T) {
	mac := mustMAC(t, "0c:ea:14:42:42:42")
	want := "2f840033-e0ce-4cf0-971a-25e61c275d07"
	id, err := NewMockIdentity(mac, "", want, net.ParseIP("192.168.1.42").To4(), 8080)
	if err != nil {
		t.Fatalf("NewMockIdentity: %v", err)
	}
	if id.GUID != want {
		t.Errorf("GUID = %q, want %q", id.GUID, want)
	}
}

func TestNewMockIdentity_InvalidGUID(t *testing.T) {
	mac := mustMAC(t, "0c:ea:14:42:42:42")
	_, err := NewMockIdentity(mac, "", "not-a-uuid", net.ParseIP("192.168.1.42").To4(), 8080)
	if err == nil {
		t.Fatal("expected error for invalid GUID, got nil")
	}
}

func TestNewMockIdentity_InvalidMACLength(t *testing.T) {
	mac := net.HardwareAddr{0x01, 0x02}
	_, err := NewMockIdentity(mac, "", "", net.ParseIP("192.168.1.42").To4(), 8080)
	if err == nil {
		t.Fatal("expected error for short MAC, got nil")
	}
}

func TestNewMockIdentity_NonIPv4(t *testing.T) {
	mac := mustMAC(t, "0c:ea:14:42:42:42")
	ipv6 := net.ParseIP("2001:db8::1")
	_, err := NewMockIdentity(mac, "", "", ipv6, 8080)
	if err == nil {
		t.Fatal("expected error for IPv6, got nil")
	}
}

func TestNewMockIdentity_ZeroPort(t *testing.T) {
	mac := mustMAC(t, "0c:ea:14:42:42:42")
	_, err := NewMockIdentity(mac, "", "", net.ParseIP("192.168.1.42").To4(), 0)
	if err == nil {
		t.Fatal("expected error for port 0, got nil")
	}
}

func TestMockIdentity_String(t *testing.T) {
	mac := mustMAC(t, "0c:ea:14:42:42:42")
	id, err := NewMockIdentity(mac, "", "", net.ParseIP("192.168.1.42").To4(), 8080)
	if err != nil {
		t.Fatalf("NewMockIdentity: %v", err)
	}
	s := id.String()
	if !strings.Contains(s, "0c:ea:14:42:42:42") {
		t.Errorf("String() %q missing MAC", s)
	}
	if !strings.Contains(s, "0cea14424242") {
		t.Errorf("String() %q missing ID", s)
	}
}

func TestNewMockIdentity_AlternateMAC(t *testing.T) {
	mac := mustMAC(t, "0c:ea:14:42:42:43")
	id, err := NewMockIdentity(mac, "", "", net.ParseIP("192.168.1.43").To4(), 8081)
	if err != nil {
		t.Fatalf("NewMockIdentity: %v", err)
	}
	if id.ID != "0cea14424243" {
		t.Errorf("ID = %q, want %q", id.ID, "0cea14424243")
	}
	if id.Name != "UA Intercom Viewer 4243" {
		t.Errorf("Name = %q, want %q", id.Name, "UA Intercom Viewer 4243")
	}
}
