// cloudca generates the mTLS material for the Saison-17 cloud
// side-channel: a self-signed ECDSA P-256 root CA plus a server
// certificate (with an IP SAN) and a client certificate, all signed
// by that CA.
//
// Usage:
//
//	go run ./cmd/cloudca -ip <vps-ip> -out ./certs
//
// Output files (in -out):
//
//	ca.crt      root CA certificate  -> distributed to BOTH sides
//	ca.key      root CA private key  -> STAYS on the issuing machine,
//	                                    never on the RPi/VPS, never in
//	                                    the repo, never in logs
//	server.crt  server cert, IP SAN = -ip  -> VPS
//	server.key  server private key         -> VPS
//	client.crt  client cert                -> RPi (edge)
//	client.key  client private key         -> RPi (edge)
//
// The server SAN is an IP SAN on purpose: the edge dials the VPS by
// its raw IP, so the standard tls.Config verification works without
// any InsecureSkipVerify hack (unlike the UDM connection, which has
// no SAN at all). If you ever reach for InsecureSkipVerify here,
// something is wrong.
//
// -ip is required and is NOT baked into the source: real
// infrastructure addresses must not live in the repository (Saison-16
// repo-incident lesson). The operator passes the VPS IP at generation
// time.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func main() {
	ipFlag := flag.String("ip", "",
		"REQUIRED: the server's public IPv4/IPv6 address, used as the "+
			"server-cert IP SAN (e.g. the VPS address). Kept out of source "+
			"on purpose - pass it explicitly.")
	outDir := flag.String("out", "./certs",
		"output directory for the six PEM files (gitignored)")
	years := flag.Int("years", 10, "certificate validity in years")
	flag.Parse()

	if *ipFlag == "" {
		log.Fatal("cloudca: -ip is required (the server IP SAN; not baked into source)")
	}
	ip := net.ParseIP(*ipFlag)
	if ip == nil {
		log.Fatalf("cloudca: -ip %q is not a valid IP address", *ipFlag)
	}

	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		log.Fatalf("cloudca: mkdir %s: %v", *outDir, err)
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(time.Duration(*years) * 365 * 24 * time.Hour)

	// 1) Root CA.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("cloudca: generate ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{Organization: []string{"carvilon cloud CA"}},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		log.Fatalf("cloudca: create ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		log.Fatalf("cloudca: parse ca cert: %v", err)
	}

	// 2) Server cert (IP SAN), signed by the CA.
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("cloudca: generate server key: %v", err)
	}
	serverTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "carvilon-cloud", Organization: []string{"carvilon cloud"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		log.Fatalf("cloudca: create server cert: %v", err)
	}

	// 3) Client cert (edge / RPi), signed by the CA.
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("cloudca: generate client key: %v", err)
	}
	clientTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "carvilon-edge", Organization: []string{"carvilon edge"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTmpl, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		log.Fatalf("cloudca: create client cert: %v", err)
	}

	writeCert(filepath.Join(*outDir, "ca.crt"), caDER)
	writeKey(filepath.Join(*outDir, "ca.key"), caKey)
	writeCert(filepath.Join(*outDir, "server.crt"), serverDER)
	writeKey(filepath.Join(*outDir, "server.key"), serverKey)
	writeCert(filepath.Join(*outDir, "client.crt"), clientDER)
	writeKey(filepath.Join(*outDir, "client.key"), clientKey)

	log.Printf("cloudca: wrote ca/server/client cert+key to %s (server IP SAN = %s, valid %d years)",
		*outDir, ip, *years)
	log.Printf("cloudca: distribute ca.crt+client.crt+client.key to the RPi, ca.crt+server.crt+server.key to the VPS; KEEP ca.key here only")
}

// serial returns a random 128-bit certificate serial number.
func serial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		log.Fatalf("cloudca: serial: %v", err)
	}
	return n
}

// writeCert writes a DER-encoded certificate as a PEM CERTIFICATE
// file with mode 0644 (public material).
func writeCert(path string, der []byte) {
	writePEM(path, &pem.Block{Type: "CERTIFICATE", Bytes: der}, 0o644)
}

// writeKey marshals an ECDSA private key as PKCS#8 and writes it as a
// PEM PRIVATE KEY file with mode 0600 (secret material).
func writeKey(path string, key *ecdsa.PrivateKey) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		log.Fatalf("cloudca: marshal key %s: %v", path, err)
	}
	writePEM(path, &pem.Block{Type: "PRIVATE KEY", Bytes: der}, 0o600)
}

func writePEM(path string, block *pem.Block, mode os.FileMode) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		log.Fatalf("cloudca: open %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, block); err != nil {
		log.Fatalf("cloudca: encode %s: %v", path, err)
	}
}
