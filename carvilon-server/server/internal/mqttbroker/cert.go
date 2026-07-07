package mqttbroker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// The broker's TLS trust is a two-cert chain, NOT a lone self-signed leaf.
// A Shelly (and other strict clients) reject a self-signed *receiver*
// certificate outright ("self-signed receiver certificates are not
// supported"), even when that exact leaf is pinned as the device's user CA -
// which is why an earlier ssl_ca=<leaf> setup failed with mbedTLS -0x2900.
// So the broker runs a tiny internal CA:
//
//   ca.crt / ca.key  - a self-signed CA (CA:TRUE, keyCertSign). Long-lived and
//                      STABLE. This is what a device pins via Shelly.PutUserCA.
//   tls.crt / tls.key - the leaf the TLS listener presents, SIGNED BY the CA,
//                      carrying the LAN SANs. It may rotate / self-heal freely
//                      (SAN drift, renewal) WITHOUT re-provisioning devices,
//                      because devices trust the CA, not the leaf.
const (
	caCertName   = "ca.crt"
	caKeyName    = "ca.key"
	leafCertName = "tls.crt"
	leafKeyName  = "tls.key"
)

// ensureCertChain makes sure an internal CA and a broker leaf signed by it
// exist under dir, with the leaf's SANs covering every host in hosts. It
// returns the leaf cert/key paths (the TLS listener presents them) and the CA
// cert path (uploaded to devices as their trust anchor). leafRegenerated is
// true when the leaf was (re-)signed this call - safe to log, but it does NOT
// require devices to re-provision (they pin the stable CA).
//
// A leaf is kept only when it is chained to THIS CA, still covers the hosts,
// and is unexpired. Anything else - a missing leaf, a SAN gap, or the legacy
// pre-CA self-signed leaf (issuer==subject, not signed by the CA) - is
// re-signed from the CA. That makes the migration off the old self-signed
// leaf automatic on the next start.
func ensureCertChain(dir string, hosts []string) (leafCert, leafKey, caCert string, leafRegenerated bool, err error) {
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", "", "", false, fmt.Errorf("mqttbroker: mkdir cert dir: %w", err)
	}
	caCert = filepath.Join(dir, caCertName)
	caKey := filepath.Join(dir, caKeyName)
	leafCert = filepath.Join(dir, leafCertName)
	leafKey = filepath.Join(dir, leafKeyName)

	ca, caPriv, err := ensureCA(caCert, caKey)
	if err != nil {
		return "", "", "", false, err
	}

	if fileExists(leafCert) && fileExists(leafKey) {
		if leaf, lerr := loadCert(leafCert); lerr == nil &&
			leaf.CheckSignatureFrom(ca) == nil && certCoversHosts(leaf, hosts) {
			return leafCert, leafKey, caCert, false, nil
		}
		// Missing key, unreadable, a SAN gap, or the legacy self-signed leaf:
		// fall through and re-sign a fresh leaf from the CA.
	}
	if err = signLeaf(ca, caPriv, leafCert, leafKey, hosts); err != nil {
		return "", "", "", false, err
	}
	return leafCert, leafKey, caCert, true, nil
}

// ensureCA loads the internal CA, generating a fresh self-signed CA cert+key
// when either file is missing or unreadable. The CA is the stable trust
// anchor devices pin, so it is generated once and left alone thereafter.
func ensureCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if fileExists(certPath) && fileExists(keyPath) {
		if cert, err := loadCert(certPath); err == nil {
			if key, kerr := loadECKey(keyPath); kerr == nil {
				return cert, key, nil
			}
		}
		// Unreadable pair: regenerate. Any leaf signed by the old CA stops
		// verifying and will be re-signed below on the same start.
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("mqttbroker: generate ca key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"carvilon mqtt broker"}, CommonName: "carvilon mqtt broker CA"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(20 * 365 * 24 * time.Hour), // stable, long-lived anchor
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true, // signs leaves only, no sub-CAs
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("mqttbroker: create ca cert: %w", err)
	}
	if err := writePEM(certPath, &pem.Block{Type: "CERTIFICATE", Bytes: der}, 0o644); err != nil {
		return nil, nil, fmt.Errorf("mqttbroker: write ca cert: %w", err)
	}
	if err := writeECKey(keyPath, key); err != nil {
		return nil, nil, fmt.Errorf("mqttbroker: write ca key: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("mqttbroker: parse ca cert: %w", err)
	}
	return cert, key, nil
}

// signLeaf writes a fresh ECDSA P-256 broker leaf signed by the CA, with the
// hosts as SANs and TLS-server-auth EKU. It replaces any existing leaf files.
func signLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, certPath, keyPath string, hosts []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("mqttbroker: generate leaf key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"carvilon mqtt broker"}, CommonName: "carvilon mqtt broker"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	applyHosts(&tmpl, hosts)
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("mqttbroker: sign leaf: %w", err)
	}
	if err := writePEM(certPath, &pem.Block{Type: "CERTIFICATE", Bytes: der}, 0o644); err != nil {
		return fmt.Errorf("mqttbroker: write leaf cert: %w", err)
	}
	if err := writeECKey(keyPath, key); err != nil {
		return fmt.Errorf("mqttbroker: write leaf key: %w", err)
	}
	return nil
}

// applyHosts classifies hosts into IP vs DNS SANs on tmpl, falling back to
// loopback when none are usable (a cert must carry at least one SAN).
func applyHosts(tmpl *x509.Certificate, hosts []string) {
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	if len(tmpl.IPAddresses) == 0 && len(tmpl.DNSNames) == 0 {
		tmpl.DNSNames = []string{"localhost"}
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	}
}

func randSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("mqttbroker: serial: %w", err)
	}
	return serial, nil
}

// loadCert reads and parses the first certificate from a PEM file.
func loadCert(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("mqttbroker: no CERTIFICATE block in %s", path)
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

func loadECKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("mqttbroker: no key block in %s", path)
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func writeECKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("mqttbroker: marshal key: %w", err)
	}
	return writePEM(path, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}, 0o600)
}

// certCoversHosts reports whether leaf serves every non-empty host in hosts
// (as an IP or DNS SAN) and is currently within its validity window.
// VerifyHostname handles IP literals against IPAddresses and names against
// DNSNames, so the check matches exactly what a client's TLS stack does.
func certCoversHosts(leaf *x509.Certificate, hosts []string) bool {
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return false
	}
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if leaf.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

// sanSummary renders a cert's SANs for a log line, e.g. "DNS=[localhost]
// IP=[192.168.1.42 127.0.0.1]" - the fastest way to eyeball a cert's names
// without reaching for openssl.
func sanSummary(leaf *x509.Certificate) string {
	ips := make([]string, 0, len(leaf.IPAddresses))
	for _, ip := range leaf.IPAddresses {
		ips = append(ips, ip.String())
	}
	return fmt.Sprintf("DNS=%v IP=%v", leaf.DNSNames, ips)
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func writePEM(path string, block *pem.Block, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, block)
}
