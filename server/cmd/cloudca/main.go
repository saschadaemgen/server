// cloudca generates the mTLS material for the Saison-17 cloud tier: a
// self-signed ECDSA P-256 root CA plus the leaf certs signed by it.
//
// Usage:
//
//	go run ./cmd/cloudca -ip <vps-ip> -out ./certs
//	go run ./cmd/cloudca -ip <vps-ip> -out ./certs -renew-ca   # full re-roll
//
// Output files (in -out):
//
//	ca.crt           root CA certificate  -> distributed to BOTH sides
//	ca.key           root CA private key  -> STAYS on the issuing machine,
//	                                         never on the RPi/VPS, never in
//	                                         the repo, never in logs
//	server.crt       side-channel server cert, IP SAN = -ip  -> VPS
//	server.key       side-channel server key                 -> VPS
//	whip-server.crt  WHIP-ingress server cert, IP SAN = -ip   -> VPS (:8444)
//	whip-server.key  WHIP-ingress server key                  -> VPS
//	client.crt       edge client cert                         -> RPi
//	client.key       edge client key                          -> RPi
//
// The server SANs are IP SANs on purpose: the edge dials the VPS by its
// raw IP, so the standard tls.Config verification works without any
// InsecureSkipVerify hack (unlike the UDM connection, which has no SAN
// at all). If you ever reach for InsecureSkipVerify here, something is
// wrong.
//
// CA REUSE: a normal run reuses an existing ca.crt/ca.key and only
// creates MISSING leaf certs - so re-running to add the WHIP cert does
// not disturb the already-distributed side-channel chain. Existing leaf
// files are left untouched (delete a file to rotate just that one).
// -renew-ca regenerates the CA and every leaf; that invalidates all
// previously distributed certs and forces a full re-roll on both sides.
//
// -ip is required and is NOT baked into the source: real infrastructure
// addresses must not live in the repository (Saison-16 repo-incident
// lesson). The operator passes the VPS IP at generation time.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
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
		"output directory for the PEM files (gitignored)")
	years := flag.Int("years", 10, "certificate validity in years")
	renewCA := flag.Bool("renew-ca", false,
		"regenerate the root CA AND all leaf certs; INVALIDATES every "+
			"previously distributed cert - full re-roll only")
	flag.Parse()

	if *ipFlag == "" {
		log.Fatal("cloudca: -ip is required (the server IP SAN; not baked into source)")
	}
	ip := net.ParseIP(*ipFlag)
	if ip == nil {
		log.Fatalf("cloudca: -ip %q is not a valid IP address", *ipFlag)
	}
	if err := run(*outDir, ip, *years, *renewCA); err != nil {
		log.Fatalf("cloudca: %v", err)
	}
}

// leafSpec describes a non-CA cert to sign.
type leafSpec struct {
	base string // file base name (<base>.crt / <base>.key)
	cn   string
	eku  x509.ExtKeyUsage
	ips  []net.IP // IP SANs (server/whip); nil for the client cert
}

// run generates (or reuses) the CA and signs the leaf certs into outDir.
// It returns an error instead of exiting so it is testable.
func run(outDir string, ip net.IP, years int, renewCA bool) error {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	notBefore := time.Now()
	notAfter := notBefore.Add(time.Duration(years) * 365 * 24 * time.Hour)

	caCrtPath := filepath.Join(outDir, "ca.crt")
	caKeyPath := filepath.Join(outDir, "ca.key")

	var caCert *x509.Certificate
	var caKey *ecdsa.PrivateKey
	var err error
	switch {
	case !renewCA && fileExists(caCrtPath) && fileExists(caKeyPath):
		caCert, caKey, err = loadCA(caCrtPath, caKeyPath)
		if err != nil {
			return fmt.Errorf("load existing CA: %w", err)
		}
		log.Printf("cloudca: reusing existing CA at %s (use -renew-ca to regenerate)", caCrtPath)
	default:
		if renewCA {
			log.Printf("cloudca: -renew-ca: regenerating CA and ALL leaf certs (previous certs become invalid)")
		}
		caCert, caKey, err = makeCA(outDir, notBefore, notAfter)
		if err != nil {
			return err
		}
		log.Printf("cloudca: generated new CA at %s", caCrtPath)
	}

	specs := []leafSpec{
		{base: "server", cn: "carvilon-cloud", eku: x509.ExtKeyUsageServerAuth, ips: []net.IP{ip}},
		{base: "whip-server", cn: "carvilon-whip", eku: x509.ExtKeyUsageServerAuth, ips: []net.IP{ip}},
		{base: "client", cn: "carvilon-edge", eku: x509.ExtKeyUsageClientAuth},
	}
	for _, s := range specs {
		if err := ensureLeaf(outDir, s, caCert, caKey, notBefore, notAfter, renewCA); err != nil {
			return err
		}
	}

	log.Printf("cloudca: done (server/whip IP SAN = %s, valid %d years)", ip, years)
	log.Printf("cloudca: VPS  <- ca.crt server.crt server.key whip-server.crt whip-server.key")
	log.Printf("cloudca: RPi  <- ca.crt client.crt client.key")
	log.Printf("cloudca: KEEP ca.key on this machine only (never RPi/VPS/repo/logs)")
	return nil
}

// makeCA generates a fresh self-signed CA and writes ca.crt/ca.key.
func makeCA(outDir string, notBefore, notAfter time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ca key: %w", err)
	}
	sn, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          sn,
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
		return nil, nil, fmt.Errorf("create ca cert: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca cert: %w", err)
	}
	if err := writeCert(filepath.Join(outDir, "ca.crt"), caDER); err != nil {
		return nil, nil, err
	}
	if err := writeKey(filepath.Join(outDir, "ca.key"), caKey); err != nil {
		return nil, nil, err
	}
	return caCert, caKey, nil
}

// loadCA reads an existing CA cert + PKCS#8 ECDSA key.
func loadCA(crtPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(crtPath)
	if err != nil {
		return nil, nil, err
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("no CERTIFICATE block in %s", crtPath)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, nil, fmt.Errorf("no PEM block in %s", keyPath)
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("%s is not an ECDSA key", keyPath)
	}
	return cert, key, nil
}

// ensureLeaf signs and writes a leaf cert+key, unless both files already
// exist and force is false (then it skips, leaving the deployed cert
// untouched).
func ensureLeaf(outDir string, s leafSpec, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, notBefore, notAfter time.Time, force bool) error {
	crtPath := filepath.Join(outDir, s.base+".crt")
	keyPath := filepath.Join(outDir, s.base+".key")
	if !force && fileExists(crtPath) && fileExists(keyPath) {
		log.Printf("cloudca: %s.crt/.key exist, skipping (use -renew-ca to regenerate everything)", s.base)
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate %s key: %w", s.base, err)
	}
	sn, err := newSerial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: s.cn, Organization: []string{"carvilon"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{s.eku},
		IPAddresses:  s.ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create %s cert: %w", s.base, err)
	}
	if err := writeCert(crtPath, der); err != nil {
		return err
	}
	return writeKey(keyPath, key)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// newSerial returns a random 128-bit certificate serial number.
func newSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	return n, nil
}

// writeCert writes a DER-encoded certificate as a PEM CERTIFICATE file
// with mode 0644 (public material).
func writeCert(path string, der []byte) error {
	return writePEM(path, &pem.Block{Type: "CERTIFICATE", Bytes: der}, 0o644)
}

// writeKey marshals an ECDSA private key as PKCS#8 and writes it as a
// PEM PRIVATE KEY file with mode 0600 (secret material).
func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key %s: %w", path, err)
	}
	return writePEM(path, &pem.Block{Type: "PRIVATE KEY", Bytes: der}, 0o600)
}

func writePEM(path string, block *pem.Block, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, block); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return nil
}
