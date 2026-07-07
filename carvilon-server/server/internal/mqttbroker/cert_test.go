package mqttbroker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCertChainLeafIsCASigned is the core regression for the Shelly-TLS bug:
// the broker must present a leaf SIGNED BY the internal CA (not a self-signed
// leaf, which a Shelly rejects), and that leaf must carry the LAN IP as a SAN.
func TestCertChainLeafIsCASigned(t *testing.T) {
	dir := t.TempDir()
	leafPath, _, caPath, regen, err := ensureCertChain(dir, []string{"192.168.1.42", "localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("ensureCertChain: %v", err)
	}
	if !regen {
		t.Fatal("first call should report the leaf was generated")
	}
	ca, err := loadCert(caPath)
	if err != nil {
		t.Fatalf("loadCert ca: %v", err)
	}
	leaf, err := loadCert(leafPath)
	if err != nil {
		t.Fatalf("loadCert leaf: %v", err)
	}
	if !ca.IsCA {
		t.Error("ca.crt must be a CA (IsCA)")
	}
	if leaf.CheckSignatureFrom(ca) != nil {
		t.Fatal("leaf must be signed by the CA (not self-signed)")
	}
	if leaf.Issuer.CommonName == leaf.Subject.CommonName {
		t.Error("leaf issuer must differ from subject (a self-signed leaf is what Shelly rejects)")
	}
	if leaf.VerifyHostname("192.168.1.42") != nil {
		t.Fatalf("leaf does not serve 192.168.1.42; SANs=%s", sanSummary(leaf))
	}
}

// TestCertChainDeviceVerifiesByIP proves the device's view end-to-end: pin the
// CA (what PutUserCA uploads), dial the broker BY IP, and the CA-signed leaf
// verifies - the exact check a Shelly performs with ssl_ca="user_ca.pem".
func TestCertChainDeviceVerifiesByIP(t *testing.T) {
	dir := t.TempDir()
	leafPath, keyPath, caPath, _, err := ensureCertChain(dir, []string{"192.168.1.42", "localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("ensureCertChain: %v", err)
	}
	keyPair, err := tls.LoadX509KeyPair(leafPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{keyPair}, MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		if c, aerr := ln.Accept(); aerr == nil {
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()

	ca, err := loadCert(caPath)
	if err != nil {
		t.Fatalf("loadCert ca: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca) // exactly what Shelly.PutUserCA(caPEM) pins on the device

	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		RootCAs: roots, ServerName: "192.168.1.42", MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("device-style dial (pin CA, verify leaf by IP) failed: %v", err)
	}
	conn.Close()
}

// TestCertChainCAStableAcrossLeafRegen proves the leaf can rotate (new SANs)
// while the CA - the thing devices pin - stays the same, so no re-provisioning
// is needed when the leaf re-signs.
func TestCertChainCAStableAcrossLeafRegen(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, _, err := ensureCertChain(dir, []string{"192.168.1.42", "localhost", "127.0.0.1"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	caBefore, err := loadCert(filepath.Join(dir, caCertName))
	if err != nil {
		t.Fatalf("loadCert ca before: %v", err)
	}
	leafBefore, err := loadCert(filepath.Join(dir, leafCertName))
	if err != nil {
		t.Fatalf("loadCert leaf before: %v", err)
	}

	// A new host forces the leaf to re-sign.
	_, _, _, regen, err := ensureCertChain(dir, []string{"192.168.1.42", "10.0.0.1", "localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !regen {
		t.Fatal("a new required SAN must re-sign the leaf")
	}
	caAfter, _ := loadCert(filepath.Join(dir, caCertName))
	leafAfter, _ := loadCert(filepath.Join(dir, leafCertName))
	if caBefore.SerialNumber.Cmp(caAfter.SerialNumber) != 0 {
		t.Fatal("CA must stay stable across a leaf re-sign (devices pin it)")
	}
	if leafBefore.SerialNumber.Cmp(leafAfter.SerialNumber) == 0 {
		t.Fatal("leaf serial should change on re-sign")
	}
	if leafAfter.CheckSignatureFrom(caAfter) != nil {
		t.Fatal("re-signed leaf must still chain to the same CA")
	}
	if leafAfter.VerifyHostname("10.0.0.1") != nil {
		t.Fatal("re-signed leaf must serve the new host")
	}
}

// TestCertChainIdempotent proves a leaf that already chains to the CA and
// covers the hosts is kept as-is (no needless re-sign).
func TestCertChainIdempotent(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"192.168.1.42", "localhost", "127.0.0.1"}
	if _, _, _, _, err := ensureCertChain(dir, hosts); err != nil {
		t.Fatalf("first: %v", err)
	}
	before, _ := loadCert(filepath.Join(dir, leafCertName))
	_, _, _, regen, err := ensureCertChain(dir, hosts)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if regen {
		t.Fatal("a valid leaf must not be re-signed")
	}
	after, _ := loadCert(filepath.Join(dir, leafCertName))
	if before.SerialNumber.Cmp(after.SerialNumber) != 0 {
		t.Fatal("leaf serial changed: needlessly re-signed")
	}
}

// TestCertChainMigratesLegacySelfSignedLeaf reproduces the deployed migration:
// an OLD self-signed leaf (from before the CA existed) is detected as not
// chained to the CA and re-signed from it on the next start.
func TestCertChainMigratesLegacySelfSignedLeaf(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"192.168.1.42", "localhost", "127.0.0.1"}

	// Plant a legacy self-signed leaf where the broker keeps its leaf.
	writeLegacySelfSigned(t, filepath.Join(dir, leafCertName), filepath.Join(dir, leafKeyName), hosts)
	legacy, err := loadCert(filepath.Join(dir, leafCertName))
	if err != nil {
		t.Fatalf("loadCert legacy: %v", err)
	}
	if legacy.Issuer.String() != legacy.Subject.String() {
		t.Fatal("precondition: planted leaf should be self-signed (issuer==subject)")
	}

	// ensureCertChain must mint a CA and re-sign the leaf from it.
	leafPath, _, caPath, regen, err := ensureCertChain(dir, hosts)
	if err != nil {
		t.Fatalf("ensureCertChain: %v", err)
	}
	if !regen {
		t.Fatal("a legacy self-signed leaf must be re-signed from the new CA")
	}
	ca, _ := loadCert(caPath)
	leaf, _ := loadCert(leafPath)
	if leaf.CheckSignatureFrom(ca) != nil {
		t.Fatal("migrated leaf must chain to the CA")
	}
	if leaf.VerifyHostname("192.168.1.42") != nil {
		t.Fatal("migrated leaf must still serve the LAN IP")
	}
}

// TestEnsureCAFailsClosedOnCorruptKey proves the CA is never silently
// re-minted: a present-but-unreadable ca.key hard-fails instead of orphaning
// every device that pinned the old CA.
func TestEnsureCAFailsClosedOnCorruptKey(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"192.168.1.42", "localhost", "127.0.0.1"}
	if _, _, _, _, err := ensureCertChain(dir, hosts); err != nil {
		t.Fatalf("first: %v", err)
	}
	caBefore, _ := loadCert(filepath.Join(dir, caCertName))

	// Corrupt ca.key (present but unparseable).
	if err := os.WriteFile(filepath.Join(dir, caKeyName), []byte("not a key\n"), 0o600); err != nil {
		t.Fatalf("corrupt ca.key: %v", err)
	}
	if _, _, _, _, err := ensureCertChain(dir, hosts); err == nil {
		t.Fatal("a present-but-unreadable CA key must fail closed, not re-mint the CA")
	}
	// The CA on disk must be untouched (not re-minted).
	caAfter, err := loadCert(filepath.Join(dir, caCertName))
	if err != nil {
		t.Fatalf("ca.crt should be untouched: %v", err)
	}
	if caBefore.SerialNumber.Cmp(caAfter.SerialNumber) != 0 {
		t.Fatal("CA cert must not be re-minted on a corrupt key")
	}
}

// TestEnsureCAFailsClosedOnHalfPresent proves a half-present CA pair (cert
// without key) fails closed rather than regenerating.
func TestEnsureCAFailsClosedOnHalfPresent(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"192.168.1.42", "localhost", "127.0.0.1"}
	if _, _, _, _, err := ensureCertChain(dir, hosts); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, caKeyName)); err != nil {
		t.Fatalf("remove ca.key: %v", err)
	}
	if _, _, _, _, err := ensureCertChain(dir, hosts); err == nil {
		t.Fatal("a half-present CA (cert, no key) must fail closed")
	}
}

// TestCertChainReSignsOnKeyMismatch proves a leaf cert sitting next to a
// mismatched key (e.g. a torn write during re-sign) is re-signed rather than
// returned as good (which would brick the whole broker at LoadX509KeyPair).
func TestCertChainReSignsOnKeyMismatch(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"192.168.1.42", "localhost", "127.0.0.1"}
	if _, _, _, _, err := ensureCertChain(dir, hosts); err != nil {
		t.Fatalf("first: %v", err)
	}
	caBefore, _ := loadCert(filepath.Join(dir, caCertName))

	// Overwrite tls.key with a DIFFERENT, valid EC key (mismatched with the leaf).
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("other key: %v", err)
	}
	if err := writeECKey(filepath.Join(dir, leafKeyName), other); err != nil {
		t.Fatalf("write mismatched key: %v", err)
	}

	leafPath, keyPath, _, regen, err := ensureCertChain(dir, hosts)
	if err != nil {
		t.Fatalf("re-sign: %v", err)
	}
	if !regen {
		t.Fatal("a leaf whose key does not match must be re-signed")
	}
	// The re-signed pair must load cleanly, and the CA must be unchanged.
	if _, err := tls.LoadX509KeyPair(leafPath, keyPath); err != nil {
		t.Fatalf("re-signed keypair must be valid: %v", err)
	}
	caAfter, _ := loadCert(filepath.Join(dir, caCertName))
	if caBefore.SerialNumber.Cmp(caAfter.SerialNumber) != 0 {
		t.Fatal("CA must stay stable while only the leaf re-signs")
	}
}

// writeLegacySelfSigned plants a self-signed leaf exactly like the pre-CA
// broker produced (issuer==subject, TLS-server EKU, LAN SANs).
func writeLegacySelfSigned(t *testing.T, certPath, keyPath string, hosts []string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("legacy key: %v", err)
	}
	serial, err := randSerial()
	if err != nil {
		t.Fatalf("legacy serial: %v", err)
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"carvilon mqtt broker"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	applyHosts(&tmpl, hosts)
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key) // self-signed
	if err != nil {
		t.Fatalf("legacy create: %v", err)
	}
	if err := writePEM(certPath, &pem.Block{Type: "CERTIFICATE", Bytes: der}, 0o644); err != nil {
		t.Fatalf("legacy write cert: %v", err)
	}
	if err := writeECKey(keyPath, key); err != nil {
		t.Fatalf("legacy write key: %v", err)
	}
}
