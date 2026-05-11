package mqtt

import (
	"bytes"
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
	"strings"
	"testing"
	"time"

	"unifix.local/mock/internal/identity"
	"unifix.local/mock/internal/state"
	"unifix.local/shared/proto"
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
	id, err := identity.NewMockIdentity(mac, "",
		"2f840033-e0ce-4cf0-971a-25e61c275d07",
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
		LocationID:     "abc-location",
		Name:           "UA Intercom Viewer 4242",
	}
}

func testBoot() bootState {
	return bootState{
		startTime: 1700000000,
		bootGUID:  "00000000-0000-4000-8000-000000000000",
	}
}

// writeCertKeyAndCA writes a self-signed cert + key + CA cert
// (same self) into dir and returns the dir. Suitable for tests
// that drive buildTLSConfig.
func writeCertKeyAndCA(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	crtPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshalkey: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	mustWrite := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustWrite("broker.crt", crtPEM)
	mustWrite("broker.key", keyPEM)
	mustWrite("broker_ca.crt", crtPEM)
	return dir
}

func TestNew_NilIdentity(t *testing.T) {
	if _, err := New(nil, testBundle(), "/tmp", silentLogger{}); err == nil {
		t.Fatal("expected error for nil identity")
	}
}

func TestNew_NilBundle(t *testing.T) {
	if _, err := New(testIdentity(t), nil, "/tmp", silentLogger{}); err == nil {
		t.Fatal("expected error for nil bundle")
	}
}

func TestNew_EmptyBrokerAddress(t *testing.T) {
	b := testBundle()
	b.BrokerAddress = ""
	if _, err := New(testIdentity(t), b, "/tmp", silentLogger{}); err == nil {
		t.Fatal("expected error for empty broker_address")
	}
}

func TestNew_EmptyControllerID(t *testing.T) {
	b := testBundle()
	b.ControllerID = ""
	if _, err := New(testIdentity(t), b, "/tmp", silentLogger{}); err == nil {
		t.Fatal("expected error for empty controller_id")
	}
}

func TestNew_NilLogger(t *testing.T) {
	if _, err := New(testIdentity(t), testBundle(), "/tmp", nil); err == nil {
		t.Fatal("expected error for nil logger")
	}
}

func TestBuildTLSConfig_LoadsClientCertAndCA(t *testing.T) {
	dir := writeCertKeyAndCA(t)
	c := &Client{certDir: dir}
	cfg, err := c.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates count = %d, want 1", len(cfg.Certificates))
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must be true so custom verifier handles chain")
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate must be set")
	}
}

func TestBuildTLSConfig_MissingClientCert(t *testing.T) {
	c := &Client{certDir: "/nonexistent/path"}
	if _, err := c.buildTLSConfig(); err == nil {
		t.Fatal("expected error for missing cert files")
	}
}

func TestBuildTLSConfig_InvalidCA(t *testing.T) {
	dir := writeCertKeyAndCA(t)
	if err := os.WriteFile(filepath.Join(dir, "broker_ca.crt"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("clobber CA: %v", err)
	}
	c := &Client{certDir: dir}
	if _, err := c.buildTLSConfig(); err == nil {
		t.Fatal("expected error for non-PEM CA")
	}
}

func TestAppendVarint_SingleByte(t *testing.T) {
	got := appendVarint(nil, 12)
	if !bytes.Equal(got, []byte{0x0c}) {
		t.Errorf("varint(12) = % x, want 0c", got)
	}
}

func TestAppendVarint_TwoBytes(t *testing.T) {
	got := appendVarint(nil, 200)
	if !bytes.Equal(got, []byte{0xc8, 0x01}) {
		t.Errorf("varint(200) = % x, want c8 01", got)
	}
}

func TestBuildHeartbeatBody_OuterWrapper(t *testing.T) {
	body := buildHeartbeatBody(testIdentity(t), testBundle(), testBoot())
	if len(body) < 4 {
		t.Fatalf("body too short: %d", len(body))
	}
	if body[0] != outerWrapper {
		t.Errorf("outer tag = 0x%02x, want 0x%02x", body[0], outerWrapper)
	}
}

func TestBuildHeartbeatBody_ContainsAdoptedTrue(t *testing.T) {
	body := buildHeartbeatBody(testIdentity(t), testBundle(), testBoot())
	if !bytes.Contains(body, []byte("adopted=true")) {
		t.Error("heartbeat body missing 'adopted=true' (UDM marks device offline within 60s)")
	}
}

func TestBuildHeartbeatBody_ContainsCriticalFields(t *testing.T) {
	id := testIdentity(t)
	bundle := testBundle()
	body := buildHeartbeatBody(id, bundle, testBoot())
	wantSubstrs := []string{
		"adopted=true",
		"mac=0c:ea:14:42:42:42",
		"ipv4=192.168.1.42",
		"controller_id=0cea14122cfd",
		"location_id=abc-location",
		"proto_version=1",
		"app_ver=v1.0",
		"hw_type=GA",
		"mode=user",
		"security_check=false",
		"port=8080",
	}
	for _, s := range wantSubstrs {
		if !bytes.Contains(body, []byte(s)) {
			t.Errorf("heartbeat body missing %q", s)
		}
	}
}

func TestHeartbeatStringFields_ExactOrder(t *testing.T) {
	fields := heartbeatStringFields(testIdentity(t), testBundle(), testBoot())
	if len(fields) != 21 {
		t.Fatalf("got %d strings, want 21", len(fields))
	}
	// First and last are pcap-fixed.
	if fields[0] != "adopted=true" {
		t.Errorf("fields[0] = %q, want adopted=true", fields[0])
	}
	if !strings.HasPrefix(fields[20], "guid=") {
		t.Errorf("fields[20] = %q, want guid=...", fields[20])
	}
}

func TestBuildHeartbeatBody_LeadFieldsPresent(t *testing.T) {
	id := testIdentity(t)
	body := buildHeartbeatBody(id, testBundle(), testBoot())
	if !bytes.Contains(body, []byte(id.ID)) {
		t.Error("body missing ID lead field")
	}
	if !bytes.Contains(body, []byte(id.Model)) {
		t.Error("body missing Model lead field")
	}
	if !bytes.Contains(body, []byte(id.Name)) {
		t.Error("body missing Name lead field")
	}
}

func TestDefaultHandler_ReturnsNonEmpty(t *testing.T) {
	h := DefaultHandler{}
	body := h.Handle("/test", "req-1", []byte{})
	if len(body) == 0 {
		t.Fatal("default handler returned empty body")
	}
	if body[0] != 0x12 {
		t.Errorf("response first byte = 0x%02x, want 0x12 (rpc outer wrapper)", body[0])
	}
}

func TestDefaultHandler_RoundtripsThroughDecode(t *testing.T) {
	h := DefaultHandler{}
	body := h.Handle("/remote_view", "abc123", nil)
	req, err := proto.DecodeRPCRequest(body)
	if err != nil {
		t.Fatalf("DecodeRPCRequest: %v", err)
	}
	if req.Path != "/remote_view" {
		t.Errorf("Path = %q, want %q", req.Path, "/remote_view")
	}
	if req.RequestID != "abc123" {
		t.Errorf("RequestID = %q, want %q", req.RequestID, "abc123")
	}
}

func TestSetHandler_Replaces(t *testing.T) {
	c, err := New(testIdentity(t), testBundle(), "/tmp", silentLogger{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	called := false
	c.SetHandler(handlerFunc(func(string, string, []byte) []byte {
		called = true
		return []byte("custom")
	}))
	got := c.handler.Handle("/x", "y", nil)
	if !called {
		t.Error("custom handler was not invoked")
	}
	if !bytes.Equal(got, []byte("custom")) {
		t.Errorf("handler returned % x, want 'custom'", got)
	}
}

func TestStats_InitiallyZero(t *testing.T) {
	c, err := New(testIdentity(t), testBundle(), "/tmp", silentLogger{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hb, recv, ans := c.Stats()
	if hb != 0 || recv != 0 || ans != 0 {
		t.Errorf("Stats = %d/%d/%d, want all zero", hb, recv, ans)
	}
}

func TestNewBootState_GeneratesFreshGUID(t *testing.T) {
	a, err := newBootState()
	if err != nil {
		t.Fatalf("newBootState: %v", err)
	}
	b, err := newBootState()
	if err != nil {
		t.Fatalf("newBootState: %v", err)
	}
	if a.bootGUID == b.bootGUID {
		t.Error("two bootStates produced identical GUIDs")
	}
	if len(a.bootGUID) != 36 {
		t.Errorf("bootGUID length = %d, want 36", len(a.bootGUID))
	}
	if a.startTime <= 0 {
		t.Error("startTime not set")
	}
}

// handlerFunc lets a single function act as an RPCHandler in tests.
type handlerFunc func(path, requestID string, body []byte) []byte

func (f handlerFunc) Handle(path, requestID string, body []byte) []byte {
	return f(path, requestID, body)
}

func TestAsciiPreview_AllPrintable(t *testing.T) {
	if got := asciiPreview([]byte("hello")); got != "hello" {
		t.Errorf("asciiPreview = %q, want %q", got, "hello")
	}
}

func TestAsciiPreview_MixedBinary(t *testing.T) {
	got := asciiPreview([]byte{0x0a, 'h', 'i', 0xff})
	if got != ".hi." {
		t.Errorf("asciiPreview = %q, want %q", got, ".hi.")
	}
}

func TestAsciiPreview_Empty(t *testing.T) {
	if got := asciiPreview(nil); got != "" {
		t.Errorf("asciiPreview(nil) = %q, want \"\"", got)
	}
	if got := asciiPreview([]byte{}); got != "" {
		t.Errorf("asciiPreview([]byte{}) = %q, want \"\"", got)
	}
}

func TestAsciiPreview_PathInBody(t *testing.T) {
	// Saison-11 diagnostic pattern: spot the path string embedded
	// inside a protobuf-ish binary frame.
	payload := append([]byte{0x0a, 0x0c}, []byte("/remote_view")...)
	payload = append(payload, 0x12, 0x05, 0x00, 0x01, 0x02, 0x03, 0x04)
	got := asciiPreview(payload)
	if !strings.Contains(got, "/remote_view") {
		t.Errorf("asciiPreview = %q, want substring %q", got, "/remote_view")
	}
}
