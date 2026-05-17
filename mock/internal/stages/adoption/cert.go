package adoption

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"
	certValidity   = 10 * 365 * 24 * time.Hour
)

// EnsureServerCert returns a TLS certificate suitable for binding
// the adoption endpoint. If server.crt and server.key already
// exist in certDir, loads them. Otherwise generates a fresh
// self-signed ECDSA P-256 cert valid for 10 years, persists it,
// and returns it.
func EnsureServerCert(certDir string, hostnames []string, ips []net.IP) (tls.Certificate, error) {
	if certDir == "" {
		return tls.Certificate{}, errors.New("cert: certDir must not be empty")
	}
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("cert: mkdir %s: %w", certDir, err)
	}

	crtPath := filepath.Join(certDir, serverCertFile)
	keyPath := filepath.Join(certDir, serverKeyFile)

	_, errCrt := os.Stat(crtPath)
	_, errKey := os.Stat(keyPath)
	if errCrt == nil && errKey == nil {
		return tls.LoadX509KeyPair(crtPath, keyPath)
	}

	return generateAndPersist(crtPath, keyPath, hostnames, ips)
}

func generateAndPersist(crtPath, keyPath string, hostnames []string, ips []net.IP) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cert: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cert: serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "carvilon-mock",
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              hostnames,
		IPAddresses:           ips,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cert: create cert: %w", err)
	}

	crtPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	if err := os.WriteFile(crtPath, crtPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("cert: write cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cert: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("cert: write key: %w", err)
	}

	return tls.LoadX509KeyPair(crtPath, keyPath)
}
