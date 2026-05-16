// Saison 14-01-FIX01: focused tests for the ESP MJPEG proxy.
//
// The original TestESPStream_ForwardsToBackendWithoutAuthHeader
// already exercises the happy path; these add:
//
//   - TestBuildBackendStreamURL exhaustively covers the helper
//     across the edge cases the saison-14-01 string-concat
//     predecessor fumbled (trailing slash, path prefix, query
//     fragment, empty backend).
//   - TestESPStreamHandler_BuildsCorrectBackendURL and friends
//     assert path / query / Authorization in separate checks
//     so a regression points at the failing field directly.
//   - TestESPStreamHandler_LogsRequestSummary captures the
//     slog output and asserts the INFO line the operator now
//     greps for in /tmp/unifix.log.
package httpserver

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildBackendStreamURL(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		profile string
		want    string
		wantErr bool
	}{
		{
			name:    "plain base + profile",
			base:    "http://127.0.0.1:1984",
			profile: "intercom_esp",
			want:    "http://127.0.0.1:1984/api/stream.mjpeg?src=intercom_esp",
		},
		{
			name:    "trailing slash on base",
			base:    "http://127.0.0.1:1984/",
			profile: "intercom_esp",
			want:    "http://127.0.0.1:1984/api/stream.mjpeg?src=intercom_esp",
		},
		{
			name:    "multiple trailing slashes",
			base:    "http://127.0.0.1:1984///",
			profile: "intercom_browser",
			want:    "http://127.0.0.1:1984/api/stream.mjpeg?src=intercom_browser",
		},
		{
			name:    "path prefix preserved",
			base:    "http://gw.example/go2rtc",
			profile: "intercom_high",
			want:    "http://gw.example/go2rtc/api/stream.mjpeg?src=intercom_high",
		},
		{
			name:    "profile with spaces gets escaped",
			base:    "http://127.0.0.1:1984",
			profile: "intercom esp",
			want:    "http://127.0.0.1:1984/api/stream.mjpeg?src=intercom+esp",
		},
		{
			name:    "fragment on base is dropped",
			base:    "http://127.0.0.1:1984/#whatever",
			profile: "intercom_esp",
			want:    "http://127.0.0.1:1984/api/stream.mjpeg?src=intercom_esp",
		},
		{
			name:    "empty base rejected",
			base:    "",
			profile: "intercom_esp",
			wantErr: true,
		},
		{
			name:    "scheme-less base rejected",
			base:    "127.0.0.1:1984",
			profile: "intercom_esp",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildBackendStreamURL(tc.base, tc.profile)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestESPStreamHandler_BuildsCorrectBackendURL(t *testing.T) {
	var sawPath string
	var sawQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=frame")
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")
	// Trailing slash on purpose so the fix's tolerance is exercised
	// even at the integration layer.
	env.srv.cfg.StreamBackendURL = backend.URL + "/"

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/stream.mjpeg", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := env.client.Do(req)
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
	if sawQuery != "src=intercom_esp" {
		t.Errorf("backend query = %q, want src=intercom_esp", sawQuery)
	}
}

func TestESPStreamHandler_StripsAuthorizationHeader(t *testing.T) {
	var sawAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=frame")
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")
	env.srv.cfg.StreamBackendURL = backend.URL

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/stream.mjpeg", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if sawAuth != "" {
		t.Errorf("backend saw Authorization=%q; want stripped", sawAuth)
	}
}

func TestESPStreamHandler_ReturnsUnauthorizedOnBadToken(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	// Set a backend so we know the 401 is from auth, not from 503.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("backend should never be hit on bad token")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	env.srv.cfg.StreamBackendURL = backend.URL

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/stream.mjpeg", nil)
	req.Header.Set("Authorization", "Bearer broken-token-xxx")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestESPStreamHandler_LogsRequestSummary(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=frame")
		_, _ = w.Write([]byte("frame"))
	}))
	defer backend.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")
	env.srv.cfg.StreamBackendURL = backend.URL

	var logBuf bytes.Buffer
	env.srv.log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/stream.mjpeg", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	logged := logBuf.String()
	for _, fragment := range []string{
		`msg="stream proxy"`,
		`route=/esp/stream.mjpeg`,
		`label=esp`,
		`profile=intercom_esp`,
		`viewer_mac=` + espTestMAC,
	} {
		if !strings.Contains(logged, fragment) {
			t.Errorf("log missing %q\nfull log:\n%s", fragment, logged)
		}
	}
	// backend URL should appear with the resolved query, not just
	// the raw base URL. slog's text handler quotes values that
	// contain "?" / "=" so the rendered key is backend="...".
	wantBackend := `backend="` + backend.URL + `/api/stream.mjpeg?src=intercom_esp"`
	if !strings.Contains(logged, wantBackend) {
		t.Errorf("log missing backend %q\nfull log:\n%s", wantBackend, logged)
	}
}
