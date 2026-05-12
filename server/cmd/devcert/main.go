// devcert generates a self-signed RSA-2048 certificate plus key
// for local TLS testing of unifix-server. NOT for production.
//
// Usage:
//
//	go run ./cmd/devcert -host localhost
//	go run ./cmd/devcert -host unifix.local,192.168.1.42 -out ./certs/
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	hostFlag := flag.String("host", "localhost",
		"comma-separated SANs (hostnames or IPv4/IPv6 addresses)")
	outDir := flag.String("out", ".",
		"output directory for cert.pem and key.pem")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		log.Fatalf("mkdir %s: %v", *outDir, err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generate key: %v", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		log.Fatalf("serial: %v", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"unifix dev"}},
		NotBefore:             now,
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range strings.Split(*hostFlag, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	if len(tmpl.DNSNames) == 0 && len(tmpl.IPAddresses) == 0 {
		log.Fatalf("no valid SANs parsed from -host=%q", *hostFlag)
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatalf("create cert: %v", err)
	}

	certPath := filepath.Join(*outDir, "cert.pem")
	keyPath := filepath.Join(*outDir, "key.pem")

	if err := writePEM(certPath, &pem.Block{Type: "CERTIFICATE", Bytes: der}, 0o644); err != nil {
		log.Fatalf("write cert: %v", err)
	}
	keyBlock := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	if err := writePEM(keyPath, keyBlock, 0o600); err != nil {
		log.Fatalf("write key: %v", err)
	}

	log.Printf("wrote %s and %s (SANs=%s)", certPath, keyPath, *hostFlag)
}

func writePEM(path string, block *pem.Block, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, block)
}
