// Saison 14-01-FIX04: stream-source validator tests.
//
// The validator decides whether a Source-URL passed to the
// /a/streams admin form is shaped like a valid go2rtc source.
// It does not talk to go2rtc; that comes next in the request
// pipeline and may surface backend-side rejections separately.
package httpserver

import (
	"errors"
	"strings"
	"testing"

	"carvilon.local/server/internal/streams"
)

func TestStreamSourceValidation_FFmpegSyntaxWithSpaces(t *testing.T) {
	src := "ffmpeg:intercom_high#video=mjpeg#width=900#height=1200#raw=-r 15 -q:v 4"
	if err := validateStreamSource(src); err != nil {
		t.Errorf("ffmpeg source with spaces in raw= rejected: %v", err)
	}
}

func TestStreamSourceValidation_FFmpegMinimal(t *testing.T) {
	if err := validateStreamSource("ffmpeg:intercom_high"); err != nil {
		t.Errorf("minimal ffmpeg source rejected: %v", err)
	}
}

func TestStreamSourceValidation_RTSPNoSpaces(t *testing.T) {
	if err := validateStreamSource("rtsp://host:1234/stream"); err != nil {
		t.Errorf("plain rtsp:// rejected: %v", err)
	}
	if err := validateStreamSource("rtsps://192.168.1.1:7441/abc-token"); err != nil {
		t.Errorf("plain rtsps:// rejected: %v", err)
	}
	if err := validateStreamSource("https://camera.local/snapshot.jpg"); err != nil {
		t.Errorf("plain https:// rejected: %v", err)
	}
}

func TestStreamSourceValidation_RTSPWithSpaces(t *testing.T) {
	err := validateStreamSource("rtsp://host/path with space")
	if err == nil {
		t.Fatal("rtsp source with literal spaces should be rejected")
	}
	if !strings.Contains(err.Error(), "Whitespace") {
		t.Errorf("expected whitespace hint, got %v", err)
	}
}

func TestStreamSourceValidation_UnknownScheme(t *testing.T) {
	cases := []string{
		"javascript:alert(1)",
		"file:///etc/passwd",
		"rtps://typo",
		"data:text/plain;base64,Zm9v",
	}
	for _, src := range cases {
		if err := validateStreamSource(src); err == nil {
			t.Errorf("expected reject for %q", src)
		}
	}
}

func TestStreamSourceValidation_EmptySource(t *testing.T) {
	for _, src := range []string{"", "   ", "\t\n"} {
		if err := validateStreamSource(src); err == nil {
			t.Errorf("expected reject for empty/whitespace %q", src)
		}
	}
}

// Saison 14-01-FIX04: when the backend rejects a source via its
// PUT-API space-check, the admin handler surfaces a friendlier
// German-language hint that points the operator at the backend-
// config workaround instead of leaving them with an opaque
// "HTTP 400: source with spaces may be insecure".
//
// Saison 15-01 re-branding: the message no longer says "go2rtc"
// since the same check could fire from the carvilon-streaming-
// server too. Wording is now backend-neutral.
func TestRewriteStreamBackendError_SourceWithSpaces(t *testing.T) {
	in := errors.New("streams: PUT /api/streams?name=foo&src=ffmpeg+...: HTTP 400: source with spaces may be insecure")
	out := rewriteStreamBackendError(in)
	if !strings.Contains(out, "Stream-Backend lehnt Source-URLs mit Leerzeichen") {
		t.Errorf("rewrite missing space-reject hint: %q", out)
	}
	if !strings.Contains(out, "Backend-Config") {
		t.Errorf("rewrite missing backend-config-workaround hint: %q", out)
	}
}

// Saison 15-01: Put on the transitional go2rtc backend returns
// ErrNotConfigured because profile CRUD migrates to the carvilon-
// streaming-server. The rewrite must surface that hint instead
// of leaking the raw "backend URL not configured" string into
// the admin UI flash.
func TestRewriteStreamBackendError_NotConfiguredMigrationHint(t *testing.T) {
	out := rewriteStreamBackendError(streams.ErrNotConfigured)
	if !strings.Contains(out, "carvilon-streaming-server") {
		t.Errorf("rewrite missing migration hint: %q", out)
	}
	if !strings.Contains(out, "strukturierte Formular") {
		t.Errorf("rewrite missing form-coming-later hint: %q", out)
	}
}

func TestRewriteStreamBackendError_PassThroughUnknown(t *testing.T) {
	in := errors.New("streams: PUT /api/streams?name=foo: connection refused")
	out := rewriteStreamBackendError(in)
	if out != in.Error() {
		t.Errorf("unknown error should pass through; got %q want %q", out, in.Error())
	}
}
