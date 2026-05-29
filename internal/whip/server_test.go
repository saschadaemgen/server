package whip

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carvilon.local/stream/internal/publishtoken"
)

// --- test plumbing ----------------------------------------------------------

// writeSelfSignedCert generates a throwaway ECDSA cert/key valid for
// 127.0.0.1 + localhost and writes them as PEM into t.TempDir(). Uses
// crypto/x509 so the test has no openssl dependency.
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "whip-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certFile)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	_ = certOut.Close()

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyOut, err := os.Create(keyFile)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	_ = keyOut.Close()
	return certFile, keyFile
}

// signToken simulates the carvilon-edge token issuer: base64url(payload-
// JSON) + "." + base64url(HMAC-SHA256(payload-bytes, key)). Reuses the
// real publishtoken.Payload shape so the wire format can't drift.
func signToken(t *testing.T, p publishtoken.Payload, key []byte) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	payloadPart := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payloadPart))
	sigPart := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payloadPart + "." + sigPart
}

// newTestServer starts a whip.Server on a dynamic loopback port with a
// throwaway cert and returns its base URL and the HMAC key. The
// listener is bound before serve() runs, so connections never race the
// accept loop.
func newTestServer(t *testing.T) (baseURL string, key []byte) {
	t.Helper()
	certFile, keyFile := writeSelfSignedCert(t)
	key = bytes.Repeat([]byte{0xAB}, 32)

	srv, err := New(Config{
		Addr:     "127.0.0.1:0",
		CertFile: certFile,
		KeyFile:  keyFile,
		HMACKey:  key,
		Logger:   log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.serve(ctx, ln) }()

	return "https://" + ln.Addr().String(), key
}

func insecureClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
		},
		Timeout: 5 * time.Second,
	}
}

func validToken(t *testing.T, sid string, key []byte) string {
	t.Helper()
	return signToken(t, publishtoken.Payload{
		SID:   sid,
		Exp:   time.Now().Add(time.Minute).Unix(),
		Nonce: "test-nonce",
	}, key)
}

// --- tests ------------------------------------------------------------------

func TestWHIP_ValidTokenReturns501(t *testing.T) {
	base, key := newTestServer(t)
	const sid = "test-mac"

	req, err := http.NewRequest(http.MethodPost, base+"/whip/"+sid, strings.NewReader("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+validToken(t, sid, key))
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "pending S2-04") {
		t.Errorf("body = %q, want it to mention pending S2-04", body)
	}
}

func TestWHIP_NoAuthReturns401(t *testing.T) {
	base, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, base+"/whip/test-mac", strings.NewReader("v=0\r\n"))
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestWHIP_GarbageTokenReturns401(t *testing.T) {
	base, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, base+"/whip/test-mac", strings.NewReader("v=0\r\n"))
	req.Header.Set("Authorization", "Bearer garbage.token")
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestWHIP_SIDMismatchReturns401(t *testing.T) {
	base, key := newTestServer(t)
	// Token signed for a different sid than the URL path.
	tok := validToken(t, "other-sid", key)
	req, _ := http.NewRequest(http.MethodPost, base+"/whip/test-mac", strings.NewReader("v=0\r\n"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestWHIP_WrongContentTypeReturns415(t *testing.T) {
	base, key := newTestServer(t)
	const sid = "test-mac"
	req, _ := http.NewRequest(http.MethodPost, base+"/whip/"+sid, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+validToken(t, sid, key))
	req.Header.Set("Content-Type", "application/json") // not SDP

	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

func TestWHIP_WrongMethodReturns405(t *testing.T) {
	base, _ := newTestServer(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPatch, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			req, _ := http.NewRequest(method, base+"/whip/test-mac", nil)
			resp, err := insecureClient().Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s status = %d, want 405", method, resp.StatusCode)
			}
		})
	}
}

func TestWHIP_MissingStreamIDReturns404(t *testing.T) {
	base, _ := newTestServer(t)
	// POST /whip/ — empty {streamID} segment does not match the
	// pattern, so ServeMux yields 404.
	req, _ := http.NewRequest(http.MethodPost, base+"/whip/", strings.NewReader("v=0\r\n"))
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestWHIP_NewRejectsIncompleteConfig guards the constructor's
// validation: missing cert/key or empty HMAC key must error.
func TestWHIP_NewRejectsIncompleteConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no cert", Config{KeyFile: "k.pem", HMACKey: []byte("x")}},
		{"no key", Config{CertFile: "c.pem", HMACKey: []byte("x")}},
		{"no hmac", Config{CertFile: "c.pem", KeyFile: "k.pem"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Errorf("New(%+v) = nil error, want error", tc.cfg)
			}
		})
	}
}
