package icedebug

import (
	"strings"
	"testing"
)

// TestMaskAddr verifies the masker shows family + a coarse prefix but
// NEVER the full address - the whole point of the package (we want
// candidate types, not leaked IPs).
func TestMaskAddr(t *testing.T) {
	tests := []struct {
		addr           string
		wantPrefix     string
		mustNotContain string // a fragment that would leak the host
	}{
		{"192.168.1.42", "v4:192.168.x.x#", "192.168.1.42"},
		{"203.0.113.5", "v4:203.0.x.x#", "203.0.113.5"},
		{"100.64.7.9", "v4:100.64.x.x#", "100.64.7.9"}, // CGNAT range stays visible as a /16
		{"2001:db8::1", "v6:", "2001:db8::1"},
		{"device-abcdef.local", "non-ip#", "device-abcdef"}, // mDNS hostname must not leak
	}
	for _, tt := range tests {
		got := maskAddr(tt.addr)
		if !strings.HasPrefix(got, tt.wantPrefix) {
			t.Errorf("maskAddr(%q) = %q, want prefix %q", tt.addr, got, tt.wantPrefix)
		}
		if strings.Contains(got, tt.mustNotContain) {
			t.Errorf("maskAddr(%q) = %q LEAKS %q", tt.addr, got, tt.mustNotContain)
		}
	}
}

// TestMaskAddr_Distinct confirms two different addresses get different
// tags (the hash suffix), so the log can tell candidates apart without
// revealing them.
func TestMaskAddr_Distinct(t *testing.T) {
	a := maskAddr("192.168.1.10")
	b := maskAddr("192.168.1.11")
	if a == b {
		t.Errorf("maskAddr collides for distinct hosts: %q == %q", a, b)
	}
}
