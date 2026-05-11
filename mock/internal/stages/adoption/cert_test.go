package adoption

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func parseCert(t *testing.T, certDir string) *x509.Certificate {
	t.Helper()
	pemBytes, err := os.ReadFile(filepath.Join(certDir, serverCertFile))
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("not a CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert
}

func TestEnsureServerCert_GeneratesOnFirstCall(t *testing.T) {
	dir := t.TempDir()
	cert, err := EnsureServerCert(dir, []string{"mock"}, []net.IP{net.ParseIP("192.168.1.42")})
	if err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Error("empty certificate returned")
	}
	for _, name := range []string{serverCertFile, serverKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected file %s: %v", name, err)
		}
	}
}

func TestEnsureServerCert_LoadsExistingCert(t *testing.T) {
	dir := t.TempDir()
	c1, err := EnsureServerCert(dir, []string{"mock"}, []net.IP{net.ParseIP("192.168.1.42")})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	c2, err := EnsureServerCert(dir, []string{"mock"}, []net.IP{net.ParseIP("192.168.1.42")})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	parsed1, _ := x509.ParseCertificate(c1.Certificate[0])
	parsed2, _ := x509.ParseCertificate(c2.Certificate[0])
	if parsed1.SerialNumber.Cmp(parsed2.SerialNumber) != 0 {
		t.Errorf("serial mismatch: first=%s second=%s", parsed1.SerialNumber, parsed2.SerialNumber)
	}
}

func TestEnsureServerCert_RegeneratesAfterDeletion(t *testing.T) {
	dir := t.TempDir()
	c1, err := EnsureServerCert(dir, []string{"mock"}, []net.IP{net.ParseIP("192.168.1.42")})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, serverCertFile)); err != nil {
		t.Fatalf("remove crt: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, serverKeyFile)); err != nil {
		t.Fatalf("remove key: %v", err)
	}
	c2, err := EnsureServerCert(dir, []string{"mock"}, []net.IP{net.ParseIP("192.168.1.42")})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	p1, _ := x509.ParseCertificate(c1.Certificate[0])
	p2, _ := x509.ParseCertificate(c2.Certificate[0])
	if p1.SerialNumber.Cmp(p2.SerialNumber) == 0 {
		t.Error("serial should differ after regeneration")
	}
}

func TestEnsureServerCert_SANsIncludeHostnameAndIP(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureServerCert(dir,
		[]string{"unifix-mock"},
		[]net.IP{net.ParseIP("192.168.1.42")}); err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	cert := parseCert(t, dir)
	foundDNS := false
	for _, n := range cert.DNSNames {
		if n == "unifix-mock" {
			foundDNS = true
		}
	}
	if !foundDNS {
		t.Errorf("DNS SAN missing 'unifix-mock', got %v", cert.DNSNames)
	}
	foundIP := false
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.ParseIP("192.168.1.42")) {
			foundIP = true
		}
	}
	if !foundIP {
		t.Errorf("IP SAN missing 192.168.1.42, got %v", cert.IPAddresses)
	}
}

func TestEnsureServerCert_ValidPeriodAround10Years(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureServerCert(dir, []string{"mock"}, []net.IP{net.ParseIP("192.168.1.42")}); err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	cert := parseCert(t, dir)
	want := certValidity
	got := cert.NotAfter.Sub(cert.NotBefore)
	delta := got - want
	if delta < -2*time.Hour || delta > 2*time.Hour {
		t.Errorf("validity = %v, want around %v", got, want)
	}
}
