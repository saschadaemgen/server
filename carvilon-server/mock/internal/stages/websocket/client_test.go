package websocket

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"carvilon.local/mock/internal/identity"
	"carvilon.local/mock/internal/state"
)

type silentLogger struct{}

func (silentLogger) Infof(string, ...any)  {}
func (silentLogger) Warnf(string, ...any)  {}
func (silentLogger) Errorf(string, ...any) {}

func testIdentity(t *testing.T) *identity.MockIdentity {
	t.Helper()
	mac, err := net.ParseMAC("0c:ea:14:42:42:42")
	if err != nil {
		t.Fatalf("parse mac: %v", err)
	}
	id, err := identity.NewMockIdentity(mac, "", "2f840033-e0ce-4cf0-971a-25e61c275d07",
		net.ParseIP("192.168.1.42").To4(), 8080)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return id
}

func testBundle() *state.Bundle {
	return &state.Bundle{
		BrokerAddress:  "tls://192.168.1.1:12812",
		BrokerCert:     "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----",
		BrokerCertCA:   "-----BEGIN CERTIFICATE-----\nfakeca\n-----END CERTIFICATE-----",
		BrokerPrivKey:  "-----BEGIN EC PRIVATE KEY-----\nfakek\n-----END EC PRIVATE KEY-----",
		ControllerID:   "0cea14122cfd",
		ControllerType: "ULP-Go",
		Extras: state.Extras{
			DoorID: "11111111-2222-3333-4444-555555555555",
		},
		Name:        "UA Intercom Viewer 4242",
		SSHPassword: "test-pw",
		SSHUser:     "ubnt",
	}
}

func writeValidCAPEM(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestNew_NilIdentity(t *testing.T) {
	if _, err := New(nil, testBundle(), "/tmp/ca.crt", silentLogger{}); err == nil {
		t.Fatal("expected error for nil identity")
	}
}

func TestNew_NilBundle(t *testing.T) {
	if _, err := New(testIdentity(t), nil, "/tmp/ca.crt", silentLogger{}); err == nil {
		t.Fatal("expected error for nil bundle")
	}
}

func TestNew_EmptyCAPath(t *testing.T) {
	if _, err := New(testIdentity(t), testBundle(), "", silentLogger{}); err == nil {
		t.Fatal("expected error for empty caCertPath")
	}
}

func TestNew_NilLogger(t *testing.T) {
	if _, err := New(testIdentity(t), testBundle(), "/tmp/ca.crt", nil); err == nil {
		t.Fatal("expected error for nil logger")
	}
}

func TestBuildTLSConfig_ValidCA(t *testing.T) {
	caPath := writeValidCAPEM(t)
	c := &Client{caCertPath: caPath}
	cfg, err := c.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must be true; custom VerifyPeerCertificate handles chain validation")
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate must be set so the CA pin still applies")
	}
}

func TestBuildTLSConfig_MissingFile(t *testing.T) {
	c := &Client{caCertPath: "/nonexistent/path/ca.crt"}
	if _, err := c.buildTLSConfig(); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestBuildTLSConfig_InvalidPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.crt")
	if err := os.WriteFile(path, []byte("not a PEM"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := &Client{caCertPath: path}
	if _, err := c.buildTLSConfig(); err == nil {
		t.Fatal("expected error for non-PEM content")
	}
}

func TestHandleMessage_Hello(t *testing.T) {
	c := &Client{log: silentLogger{}}
	c.handleMessage(map[string]any{"msg_type": "Hello"})
	if c.helloCount != 1 {
		t.Errorf("helloCount = %d, want 1", c.helloCount)
	}
	if c.eventCount != 0 {
		t.Errorf("eventCount = %d, want 0", c.eventCount)
	}
	if c.messageCount != 1 {
		t.Errorf("messageCount = %d, want 1", c.messageCount)
	}
}

func TestHandleMessage_Event(t *testing.T) {
	c := &Client{log: silentLogger{}}
	c.handleMessage(map[string]any{"msg_type": "access.data.config"})
	if c.eventCount != 1 {
		t.Errorf("eventCount = %d, want 1", c.eventCount)
	}
	if c.helloCount != 0 {
		t.Errorf("helloCount = %d, want 0", c.helloCount)
	}
}

func TestHandleMessage_MissingType(t *testing.T) {
	c := &Client{log: silentLogger{}}
	c.handleMessage(map[string]any{"payload": "stuff"})
	if c.eventCount != 1 {
		t.Errorf("eventCount = %d, want 1 (no type falls through to default)", c.eventCount)
	}
}

func TestHandleMessage_TypeFallbackField(t *testing.T) {
	// "type" is the fallback when "msg_type" is absent.
	c := &Client{log: silentLogger{}}
	c.handleMessage(map[string]any{"type": "hello-something"})
	if c.helloCount != 1 {
		t.Errorf("helloCount = %d, want 1 (matched via 'type' fallback)", c.helloCount)
	}
}

func TestStats_InitiallyZero(t *testing.T) {
	c, err := New(testIdentity(t), testBundle(), "/tmp/ca.crt", silentLogger{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	connects, msgs, hellos, events := c.Stats()
	if connects != 0 || msgs != 0 || hellos != 0 || events != 0 {
		t.Errorf("Stats = %d/%d/%d/%d, want all zero", connects, msgs, hellos, events)
	}
}

func TestStats_AfterMessages(t *testing.T) {
	c := &Client{log: silentLogger{}}
	c.handleMessage(map[string]any{"msg_type": "Hello"})
	c.handleMessage(map[string]any{"msg_type": "access.data.config"})
	c.handleMessage(map[string]any{"msg_type": "Hello"})
	_, msgs, hellos, events := c.Stats()
	if msgs != 3 {
		t.Errorf("messageCount = %d, want 3", msgs)
	}
	if hellos != 2 {
		t.Errorf("helloCount = %d, want 2", hellos)
	}
	if events != 1 {
		t.Errorf("eventCount = %d, want 1", events)
	}
}

func TestDispatchFrame_PlainStringHello(t *testing.T) {
	c := &Client{log: silentLogger{}}
	// UDM heartbeat is the bare JSON string "Hello", encoded
	// on the wire as the 7 bytes `"Hello"`.
	c.dispatchFrame([]byte(`"Hello"`))
	if c.helloCount != 1 {
		t.Errorf("helloCount = %d, want 1", c.helloCount)
	}
	if c.eventCount != 0 {
		t.Errorf("eventCount = %d, want 0", c.eventCount)
	}
}

func TestDispatchFrame_ObjectEvent(t *testing.T) {
	c := &Client{log: silentLogger{}}
	c.dispatchFrame([]byte(`{"msg_type":"access.data.config","x":1}`))
	if c.eventCount != 1 {
		t.Errorf("eventCount = %d, want 1", c.eventCount)
	}
	if c.helloCount != 0 {
		t.Errorf("helloCount = %d, want 0", c.helloCount)
	}
}

func TestDispatchFrame_NonJSON(t *testing.T) {
	c := &Client{log: silentLogger{}}
	c.dispatchFrame([]byte("not json at all"))
	if c.eventCount != 1 {
		t.Errorf("eventCount = %d, want 1 (non-JSON treated as string event)", c.eventCount)
	}
}

func TestHandleStringMessage_Hello(t *testing.T) {
	c := &Client{log: silentLogger{}}
	c.handleStringMessage("Hello")
	if c.helloCount != 1 {
		t.Errorf("helloCount = %d, want 1", c.helloCount)
	}
}

func TestHandleStringMessage_Other(t *testing.T) {
	c := &Client{log: silentLogger{}}
	c.handleStringMessage("goodbye")
	if c.eventCount != 1 {
		t.Errorf("eventCount = %d, want 1 (non-hello string is an event)", c.eventCount)
	}
	if c.helloCount != 0 {
		t.Errorf("helloCount = %d, want 0", c.helloCount)
	}
}
