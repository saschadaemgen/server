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

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/publishtoken"
	"carvilon.local/stream/internal/streamhub"
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

// testEgressKey is the WHEP egress-auth key used by the test servers,
// DISTINCT from the publish key (0xAB) so a publish-key-signed token is
// rejected on the egress (key separation). whepSubscribe signs its Bearer
// egress token with this key.
var testEgressKey = bytes.Repeat([]byte{0xCD}, 32)

// newTestServer starts a whip.Server on a dynamic loopback port with a
// throwaway cert and returns its base URL and the HMAC key. The
// listener is bound before serve() runs, so connections never race the
// accept loop. The egress key is wired (testEgressKey) so WHEP subscribes
// must present a valid egress token (whepSubscribe does).
func newTestServer(t *testing.T) (baseURL string, key []byte) {
	t.Helper()
	base, key, _ := newTestServerH(t)
	return base, key
}

// newTestServerH is newTestServer that also returns the *Server handle, so a
// test can inspect server-side state (e.g. ConsumerCounts, S20).
func newTestServerH(t *testing.T) (baseURL string, key []byte, srv *Server) {
	t.Helper()
	certFile, keyFile := writeSelfSignedCert(t)
	key = bytes.Repeat([]byte{0xAB}, 32)

	srv, err := New(Config{
		Addr:          "127.0.0.1:0",
		CertFile:      certFile,
		KeyFile:       keyFile,
		HMACKey:       key,
		EgressHMACKey: testEgressKey,
		Hub:           streamhub.NewHub(),
		Logger:        log.New(io.Discard, "", 0),
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

	return "https://" + ln.Addr().String(), key, srv
}

// newTestServerWithTrigger is newTestServer plus a wired RequestPublish
// callback (the cold-start WHEP trigger) on a caller-supplied hub, so a test
// can dock a publisher into that hub after the trigger fires.
func newTestServerWithTrigger(t *testing.T, hub *streamhub.Hub, requestPublish func(context.Context, string) int) (baseURL string, key []byte) {
	t.Helper()
	certFile, keyFile := writeSelfSignedCert(t)
	key = bytes.Repeat([]byte{0xAB}, 32)

	srv, err := New(Config{
		Addr:           "127.0.0.1:0",
		CertFile:       certFile,
		KeyFile:        keyFile,
		HMACKey:        key,
		EgressHMACKey:  testEgressKey,
		Hub:            hub,
		Logger:         log.New(io.Discard, "", 0),
		RequestPublish: requestPublish,
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

// clientOffer builds a synthetic pion publisher: a PeerConnection with
// one H.264 send track, and returns its gathered SDP offer. Models the
// edge WHIP client (whose Go implementation lands in a later commit).
func clientOffer(t *testing.T) string {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "pion",
	)
	if err != nil {
		t.Fatalf("client track: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("add track: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gather
	return pc.LocalDescription().SDP
}

// TestWHIPHandshake drives a real WHIP handshake with a synthetic pion
// publisher: valid token + real SDP offer -> 201 Created, SDP answer,
// Location header.
func TestWHIPHandshake(t *testing.T) {
	base, key := newTestServer(t)
	const sid = "test-mac"

	req, _ := http.NewRequest(http.MethodPost, base+"/whip/"+sid, strings.NewReader(clientOffer(t)))
	req.Header.Set("Authorization", "Bearer "+validToken(t, sid, key))
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/sdp" {
		t.Errorf("Content-Type = %q, want application/sdp", ct)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/whip/"+sid+"/session/") {
		t.Errorf("Location = %q, want prefix /whip/%s/session/", loc, sid)
	}
	if !strings.Contains(string(body), "v=0") {
		t.Errorf("answer body is not SDP: %q", body)
	}
}

// TestWHIPConflict asserts the single-publisher invariant: a second
// publish for an already-active streamID is rejected with 409.
func TestWHIPConflict(t *testing.T) {
	base, key := newTestServer(t)
	const sid = "test-mac"
	tok := validToken(t, sid, key)

	publish := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, base+"/whip/"+sid, strings.NewReader(clientOffer(t)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/sdp")
		resp, err := insecureClient().Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		return resp
	}

	r1 := publish()
	defer func() { _ = r1.Body.Close() }()
	if r1.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(r1.Body)
		t.Fatalf("first publish status = %d, want 201; body=%s", r1.StatusCode, body)
	}

	r2 := publish()
	defer func() { _ = r2.Body.Close() }()
	if r2.StatusCode != http.StatusConflict {
		t.Errorf("second publish status = %d, want 409", r2.StatusCode)
	}
}

// TestWHIP_ValidTokenMalformedSDPReturns500 covers the path where auth
// succeeds but the SDP offer is unparseable: AcceptPublisher fails and
// the handler returns 500 (not a leak of the auth-vs-setup distinction
// — both are server-side concerns past the 401 gate).
func TestWHIP_ValidTokenMalformedSDPReturns500(t *testing.T) {
	base, key := newTestServer(t)
	const sid = "test-mac"

	req, _ := http.NewRequest(http.MethodPost, base+"/whip/"+sid, strings.NewReader("this is not a valid sdp offer"))
	req.Header.Set("Authorization", "Bearer "+validToken(t, sid, key))
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (malformed offer)", resp.StatusCode)
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
	hub := streamhub.NewHub()
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no cert", Config{KeyFile: "k.pem", HMACKey: []byte("x"), Hub: hub}},
		{"no key", Config{CertFile: "c.pem", HMACKey: []byte("x"), Hub: hub}},
		{"no hmac", Config{CertFile: "c.pem", KeyFile: "k.pem", Hub: hub}},
		{"no hub", Config{CertFile: "c.pem", KeyFile: "k.pem", HMACKey: []byte("x")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Errorf("New(%+v) = nil error, want error", tc.cfg)
			}
		})
	}
}
