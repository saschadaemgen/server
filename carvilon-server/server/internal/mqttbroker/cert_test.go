package mqttbroker

import (
	"path/filepath"
	"testing"
)

// TestSelfSignedCarriesLANIP is the regression for the Shelly-TLS bug: a cert
// generated with the LAN IP among its hosts must carry that IP as an IP SAN,
// or a device dialing the broker by IP fails hostname verification.
func TestSelfSignedCarriesLANIP(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "tls.crt")
	key := filepath.Join(dir, "tls.key")

	gen, err := ensureSelfSigned(cert, key, []string{"192.168.1.42", "localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("ensureSelfSigned: %v", err)
	}
	if !gen {
		t.Fatal("first generation should report generated=true")
	}
	leaf, err := loadCert(cert)
	if err != nil {
		t.Fatalf("loadCert: %v", err)
	}
	if leaf.VerifyHostname("192.168.1.42") != nil {
		t.Fatalf("cert does not serve 192.168.1.42; SANs=%s", sanSummary(leaf))
	}
}

// TestSelfSignedIsIdempotent proves a cert that already covers the hosts is
// NOT regenerated - regeneration would invalidate devices' pinned CA.
func TestSelfSignedIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "tls.crt")
	key := filepath.Join(dir, "tls.key")
	hosts := []string{"192.168.1.42", "localhost", "127.0.0.1"}

	if _, err := ensureSelfSigned(cert, key, hosts); err != nil {
		t.Fatalf("first: %v", err)
	}
	first, err := loadCert(cert)
	if err != nil {
		t.Fatalf("loadCert: %v", err)
	}
	gen, err := ensureSelfSigned(cert, key, hosts)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if gen {
		t.Fatal("a cert that already covers the hosts must not be regenerated")
	}
	second, err := loadCert(cert)
	if err != nil {
		t.Fatalf("loadCert: %v", err)
	}
	if first.SerialNumber.Cmp(second.SerialNumber) != 0 {
		t.Fatal("serial changed: the cert was needlessly regenerated")
	}
}

// TestStaleCertSelfHeals reproduces the deployed failure: a cert first
// generated WITHOUT the LAN IP (e.g. CARVILON_SERVER_IPV4 unset at the time)
// must be regenerated once the LAN IP becomes a required host.
func TestStaleCertSelfHeals(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "tls.crt")
	key := filepath.Join(dir, "tls.key")

	// First run: LAN IP empty -> only localhost/127.0.0.1 SANs (the stale cert).
	if _, err := ensureSelfSigned(cert, key, []string{"", "localhost", "127.0.0.1"}); err != nil {
		t.Fatalf("stale gen: %v", err)
	}
	stale, err := loadCert(cert)
	if err != nil {
		t.Fatalf("loadCert: %v", err)
	}
	if stale.VerifyHostname("192.168.1.42") == nil {
		t.Fatal("precondition: stale cert should NOT already serve the LAN IP")
	}

	// Second run: LAN IP now known -> the stale cert must self-heal.
	gen, err := ensureSelfSigned(cert, key, []string{"192.168.1.42", "localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("heal gen: %v", err)
	}
	if !gen {
		t.Fatal("a cert missing the LAN-IP SAN must be regenerated, not kept")
	}
	healed, err := loadCert(cert)
	if err != nil {
		t.Fatalf("loadCert: %v", err)
	}
	if healed.VerifyHostname("192.168.1.42") != nil {
		t.Fatalf("healed cert still does not serve 192.168.1.42; SANs=%s", sanSummary(healed))
	}
}
