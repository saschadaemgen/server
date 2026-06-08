package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func readFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func parseCert(t *testing.T, dir, name string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(readFile(t, dir, name))
	if block == nil {
		t.Fatalf("no PEM block in %s", name)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return c
}

func TestRun_GeneratesEightFiles(t *testing.T) {
	dir := t.TempDir()
	if err := run(dir, net.ParseIP("127.0.0.1"), 1, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, f := range []string{
		"ca.crt", "ca.key",
		"server.crt", "server.key",
		"client.crt", "client.key",
		"whip-server.crt", "whip-server.key",
	} {
		if !fileExists(filepath.Join(dir, f)) {
			t.Errorf("missing output file %s", f)
		}
	}
}

func TestRun_WhipCertChainsAndHasIPSAN(t *testing.T) {
	dir := t.TempDir()
	if err := run(dir, net.ParseIP("127.0.0.1"), 1, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	caCert, _, err := loadCA(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("loadCA: %v", err)
	}
	whip := parseCert(t, dir, "whip-server.crt")

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := whip.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("whip cert does not chain to the CA: %v", err)
	}
	if err := whip.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("whip cert missing IP SAN 127.0.0.1: %v", err)
	}
}

func TestRun_ReuseCADoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	ip := net.ParseIP("127.0.0.1")
	if err := run(dir, ip, 1, false); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	beforeCA := readFile(t, dir, "ca.crt")
	beforeCAKey := readFile(t, dir, "ca.key")
	beforeServer := readFile(t, dir, "server.crt")

	// Second run without -renew-ca: CA untouched, existing leaves
	// untouched (skip-if-exists).
	if err := run(dir, ip, 1, false); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if !bytes.Equal(beforeCA, readFile(t, dir, "ca.crt")) {
		t.Error("ca.crt changed on a second run without -renew-ca")
	}
	if !bytes.Equal(beforeCAKey, readFile(t, dir, "ca.key")) {
		t.Error("ca.key changed on a second run without -renew-ca")
	}
	if !bytes.Equal(beforeServer, readFile(t, dir, "server.crt")) {
		t.Error("server.crt changed on a second run (skip-if-exists expected)")
	}

	// The "add a missing leaf later" workflow: delete the WHIP files,
	// re-run, and only they are recreated - the CA stays put.
	if err := os.Remove(filepath.Join(dir, "whip-server.crt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "whip-server.key")); err != nil {
		t.Fatal(err)
	}
	if err := run(dir, ip, 1, false); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if !fileExists(filepath.Join(dir, "whip-server.crt")) {
		t.Error("whip-server.crt not recreated after deletion")
	}
	if !bytes.Equal(beforeCA, readFile(t, dir, "ca.crt")) {
		t.Error("ca.crt changed while recreating a deleted leaf")
	}
}

func TestRun_RenewCARegeneratesCA(t *testing.T) {
	dir := t.TempDir()
	ip := net.ParseIP("127.0.0.1")
	if err := run(dir, ip, 1, false); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	beforeCA := readFile(t, dir, "ca.crt")

	if err := run(dir, ip, 1, true); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if bytes.Equal(beforeCA, readFile(t, dir, "ca.crt")) {
		t.Error("ca.crt unchanged after -renew-ca")
	}
}
