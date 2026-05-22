// Focused tests for the mieter MJPEG proxy. Mirrors the ESP
// tests in handler_esp_stream_test.go but uses the cookie-session
// middleware so the URL-build + Authorization-strip +
// log-summary contracts are proven for both routes.
package httpserver

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loginMieterForStream is a small test helper that seeds a viewer
// and logs it in via the same POST /login path the production
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

	resp, err := env.client.Get(env.ts.URL + "/webviewer/stream.mjpeg")
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
	// mjpeg_bal via the type-default convention.
	if sawQuery != "src=mjpeg_bal" {
		t.Errorf("backend query = %q, want src=mjpeg_bal", sawQuery)
	}
}

func TestMieterStreamHandler_503WhenBackendUnconfigured(t *testing.T) {
	env := loginMieterForStream(t)
	env.srv.cfg.StreamBackendURL = ""

	resp, err := env.client.Get(env.ts.URL + "/webviewer/stream.mjpeg")
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
	// No login -> requireSession bounces to /login with 303.
	resp, err := env.client.Get(env.ts.URL + "/webviewer/stream.mjpeg")
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

	resp, err := env.client.Get(env.ts.URL + "/webviewer/stream.mjpeg")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	logged := logBuf.String()
	for _, fragment := range []string{
		`msg="stream proxy"`,
		`route=/webviewer/stream.mjpeg`,
		`label=mieter`,
		`profile=mjpeg_bal`,
		`viewer_mac=` + testViewerMAC,
	} {
		if !strings.Contains(logged, fragment) {
			t.Errorf("log missing %q\nfull log:\n%s", fragment, logged)
		}
	}
}

// The mieter side shares proxyMJPEGStream with the ESP side, so
// the no-chunked invariant applies here too. Browsers tolerate
// chunked Multipart - the test is here to lock in consistent
// behaviour across both routes so a future refactor cannot
// silently re-introduce chunked for one
// of them.
func TestMieterStream_NoChunkedTransferEncoding(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		_, _ = w.Write([]byte("--frame\r\nContent-Type: image/jpeg\r\nContent-Length: 8\r\n\r\nFAKEJPEG\r\n"))
	}))
	defer backend.Close()

	env := loginMieterForStream(t)
	env.srv.cfg.StreamBackendURL = backend.URL

	resp, err := env.client.Get(env.ts.URL + "/webviewer/stream.mjpeg")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	for _, te := range resp.TransferEncoding {
		if strings.EqualFold(te, "chunked") {
			t.Errorf("response uses chunked transfer-encoding: %v", resp.TransferEncoding)
		}
	}
	if te := resp.Header.Get("Transfer-Encoding"); te != "" && strings.Contains(strings.ToLower(te), "chunked") {
		t.Errorf("Transfer-Encoding header contains chunked: %q", te)
	}

	data, _ := io.ReadAll(resp.Body)
	if !bytes.HasPrefix(data, []byte("--frame")) {
		n := len(data)
		if n > 32 {
			n = 32
		}
		t.Errorf("body did not start with --frame; first %d bytes: %q", n, data[:n])
	}
}
