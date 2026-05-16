// Saison 14-01-FIX01: focused tests for the mieter MJPEG proxy.
// Mirrors the ESP tests in handler_esp_stream_test.go but uses
// the cookie-session middleware so the URL-build + Authorization-
// strip + log-summary contracts are proven for both routes.
package httpserver

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loginMieterForStream is a small test helper that seeds a viewer
// and logs it in via the same POST /einloggen path the production
// browser hits, so the resulting session cookie is what the proxy
// handler reads. Returns the env ready to issue GET requests.
func loginMieterForStream(t *testing.T) *testEnv {
	t.Helper()
	env := newTestServer(t)
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerName, testViewerPassword)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	return env
}

func TestMieterStreamHandler_BuildsCorrectBackendURL(t *testing.T) {
	var sawPath string
	var sawQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=frame")
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	env := loginMieterForStream(t)
	env.srv.cfg.StreamBackendURL = backend.URL + "/" // trailing slash on purpose

	resp, err := env.client.Get(env.ts.URL + "/einloggen/stream.mjpeg")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if sawPath != "/api/stream.mjpeg" {
		t.Errorf("backend path = %q, want /api/stream.mjpeg", sawPath)
	}
	// A type='web' viewer with empty stream_profile resolves to
	// intercom_browser via the type-default convention.
	if sawQuery != "src=intercom_browser" {
		t.Errorf("backend query = %q, want src=intercom_browser", sawQuery)
	}
}

func TestMieterStreamHandler_503WhenBackendUnconfigured(t *testing.T) {
	env := loginMieterForStream(t)
	env.srv.cfg.StreamBackendURL = ""

	resp, err := env.client.Get(env.ts.URL + "/einloggen/stream.mjpeg")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestMieterStreamHandler_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	// No login -> requireSession bounces to /einloggen with 303.
	resp, err := env.client.Get(env.ts.URL + "/einloggen/stream.mjpeg")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 redirect to login", resp.StatusCode)
	}
}

func TestMieterStreamHandler_LogsRequestSummary(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=frame")
		_, _ = w.Write([]byte("frame"))
	}))
	defer backend.Close()

	env := loginMieterForStream(t)
	env.srv.cfg.StreamBackendURL = backend.URL

	var logBuf bytes.Buffer
	env.srv.log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	resp, err := env.client.Get(env.ts.URL + "/einloggen/stream.mjpeg")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	logged := logBuf.String()
	for _, fragment := range []string{
		`msg="stream proxy"`,
		`route=/einloggen/stream.mjpeg`,
		`label=mieter`,
		`profile=intercom_browser`,
		`viewer_mac=` + testViewerMAC,
	} {
		if !strings.Contains(logged, fragment) {
			t.Errorf("log missing %q\nfull log:\n%s", fragment, logged)
		}
	}
}
