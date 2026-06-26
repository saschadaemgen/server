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

// ensureSelfSigned makes sure certPath and keyPath both exist,
// generating a self-signed ECDSA P-256 certificate (SANs from hosts)
// when either is missing. LAN clients pin or trust this cert. It is
// only created on first run; an operator-provided cert/key pair is
// left untouched. Returns true if a new cert was generated.
func ensureSelfSigned(certPath, keyPath string, hosts []string) (generated bool, err error) {
	if fileExists(certPath) && fileExists(keyPath) {
		return false, nil
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
