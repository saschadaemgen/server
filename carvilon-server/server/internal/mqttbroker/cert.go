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

// ensureSelfSigned makes sure certPath and keyPath both exist AND that the
// certificate's SANs cover every host in hosts, generating a self-signed
// ECDSA P-256 certificate (SANs from hosts) when either file is missing or
// the existing cert is stale. "Stale" means the cert predates a host it now
// must serve - most importantly the LAN IP a device dials over TLS: a cert
// first generated before CARVILON_SERVER_IPV4 was set carries only
// localhost/127.0.0.1 as SANs, so a device dialing the broker by its LAN IP
// fails hostname verification even though it trusts the (pinned) issuer.
// Because generation used to be strictly once-only, such a cert never
// healed; now a missing SAN triggers regeneration. LAN clients pin or trust
// this cert. An operator-provided cert/key pair never reaches here (the
// caller returns before us), so it is left untouched. Returns true if a new
// cert was generated - the caller logs it, since regeneration invalidates
// the leaf already pinned on provisioned devices (they must re-provision).
func ensureSelfSigned(certPath, keyPath string, hosts []string) (generated bool, err error) {
	if fileExists(certPath) && fileExists(keyPath) {
		if leaf, lerr := loadCert(certPath); lerr == nil && certCoversHosts(leaf, hosts) {
			return false, nil
		}
		// Missing file-pair member, unreadable, or a SAN gap: fall through
		// and regenerate so the SANs cover the current hosts.
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return false, fmt.Errorf("mqttbroker: mkdir cert dir: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, fmt.Errorf("mqttbroker: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return false, fmt.Errorf("mqttbroker: serial: %w", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"carvilon mqtt broker"}},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
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

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return false, fmt.Errorf("mqttbroker: create cert: %w", err)
	}
	if err := writePEM(certPath, &pem.Block{Type: "CERTIFICATE", Bytes: der}, 0o644); err != nil {
		return false, fmt.Errorf("mqttbroker: write cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return false, fmt.Errorf("mqttbroker: marshal key: %w", err)
	}
	if err := writePEM(keyPath, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}, 0o600); err != nil {
		return false, fmt.Errorf("mqttbroker: write key: %w", err)
	}
	return true, nil
}

// loadCert reads and parses the leaf certificate from a PEM file.
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
// IP=[192.168.1.42 127.0.0.1]" - the fastest way to spot a stale cert
// (missing the LAN IP) without reaching for openssl.
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
