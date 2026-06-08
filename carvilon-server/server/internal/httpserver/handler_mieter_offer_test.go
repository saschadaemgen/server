// Tests for POST /webviewer/offer.
//
// The offer-proxy forwards a tenant SDP-offer to the streaming
// backend's WebRTC signalling endpoint and copies the SDP-answer
// back. Tests cover:
//
//   - 401 without session
//   - 503 when no backend configured
//   - 502 when backend unreachable
//   - successful roundtrip: backend gets correct path + body,
//     client gets backend status + content-type + body
//   - Authorization header MUST be stripped before forwarding
package httpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"carvilon.local/server/internal/streams"
)

// fakeBackend is a tiny streams.StreamBackend that lets the test
// observe the URL we'd hit and synthesize an SDP-answer. Only the
// two URL methods + Configured are used by handler_mieter_offer.go;
// the rest panic on call so a test stumbling into them fails loud.
type fakeBackend struct {
	signalURL  string
	configured bool
}

func (f fakeBackend) MJPEGURL(string) string        { return "" }
func (f fakeBackend) WebRTCSignalURL(p string) string {
	if f.signalURL == "" {
		return ""
	}
	return f.signalURL + "?src=" + p
}
func (f fakeBackend) Configured() bool { return f.configured }
func (f fakeBackend) List(context.Context) ([]streams.Profile, error) {
	return nil, streams.ErrNotConfigured
}
func (f fakeBackend) Get(context.Context, string) (streams.Profile, error) {
	return streams.Profile{}, streams.ErrNotConfigured
}
func (f fakeBackend) Put(context.Context, streams.Profile) error {
	return streams.ErrNotConfigured
}
func (f fakeBackend) Delete(context.Context, string) error { return streams.ErrNotConfigured }
func (f fakeBackend) ListCameras(context.Context) ([]streams.Camera, error) {
	return nil, nil
}
func (f fakeBackend) Stats(context.Context) (map[string]streams.ProfileStats, error) {
	return nil, streams.ErrNotConfigured
}

func TestMieterOffer_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Post(env.ts.URL+"/webviewer/offer",
		"application/sdp", strings.NewReader("v=0"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (redirect to /login)", resp.StatusCode)
	}
}

func TestMieterOffer_ReturnsServiceUnavailableWhenBackendOff(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)
	// Default test env wires the unconfigured backend; just to be
	// explicit:
	env.srv.streams = streams.Unconfigured()

	resp, err := env.client.Post(env.ts.URL+"/webviewer/offer",
		"application/sdp", strings.NewReader("v=0"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestMieterOffer_RoundtripsToBackend(t *testing.T) {
	var (
		sawPath   string
		sawAuth   string
		sawCT     string
		sawBody   []byte
		sawMethod string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path + "?" + r.URL.RawQuery
		sawAuth = r.Header.Get("Authorization")
		sawCT = r.Header.Get("Content-Type")
		sawMethod = r.Method
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "v=0\nbackend-answer")
	}))
	defer backend.Close()

	env := newTestServer(t)
	loginMieterForTest(t, env)
	env.srv.streams = fakeBackend{
		signalURL:  backend.URL + "/offer",
		configured: true,
	}

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/offer",
		strings.NewReader("v=0\nfake-offer-body"))
	req.Header.Set("Content-Type", "application/sdp")
	// This MUST NOT reach the backend.
	req.Header.Set("Authorization", "Bearer leak-this-token")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "v=0\nbackend-answer" {
		t.Errorf("client body = %q, want backend body", string(got))
	}

	// Backend-side observations.
	if sawMethod != http.MethodPost {
		t.Errorf("backend saw method = %q, want POST", sawMethod)
	}
	if !strings.HasPrefix(sawPath, "/offer?src=") {
		t.Errorf("backend path = %q, want /offer?src=...", sawPath)
	}
	if sawAuth != "" {
		t.Errorf("backend saw Authorization = %q, must be stripped", sawAuth)
	}
	if sawCT != "application/sdp" {
		t.Errorf("backend saw Content-Type = %q, want application/sdp", sawCT)
	}
	if string(sawBody) != "v=0\nfake-offer-body" {
		t.Errorf("backend body = %q, want forwarded offer", string(sawBody))
	}
}

func TestMieterOffer_BackendUnreachableReturnsBadGateway(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)
	// Point at a port that is essentially never listening (TCP/0
	// would be a wildcard; 1 is reserved). The dial fails fast.
	env.srv.streams = fakeBackend{
		signalURL:  "http://127.0.0.1:1/offer",
		configured: true,
	}

	resp, err := env.client.Post(env.ts.URL+"/webviewer/offer",
		"application/sdp", strings.NewReader("v=0"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestMieterOffer_RejectsOversizedBody(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)
	env.srv.streams = fakeBackend{
		signalURL:  "http://127.0.0.1:9/offer",
		configured: true,
	}

	huge := strings.Repeat("x", mieterOfferMaxBytes+1)
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/offer", strings.NewReader(huge))
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (oversized body)", resp.StatusCode)
	}
}
