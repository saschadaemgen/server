package streams

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRejectsEmptyURL(t *testing.T) {
	if _, err := New("  "); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestMJPEGURLEncodesProfile(t *testing.T) {
	c, err := New("http://127.0.0.1:1984/")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got := c.MJPEGURL("intercom esp")
	want := "http://127.0.0.1:1984/api/stream.mjpeg?src=intercom+esp"
	if got != want {
		t.Fatalf("MJPEGURL: got %q want %q", got, want)
	}
}

// The seam reserves /offer?src=<profile> for the browser WebRTC
// signalling POST. WebRTCSignalURL must build the same path
// against the backend so the proxy handler can copy the body
// straight through.
func TestWebRTCSignalURLEncodesProfile(t *testing.T) {
	c, _ := New("http://127.0.0.1:8555/")
	got := c.WebRTCSignalURL("browser hd")
	want := "http://127.0.0.1:8555/offer?src=browser+hd"
	if got != want {
		t.Fatalf("WebRTCSignalURL: got %q want %q", got, want)
	}
}

// Configured() always returns true for a constructed Client; the
// unconfiguredBackend in backend.go returns false. Handlers gate
// on this instead of nil-checking the Client.
func TestConfiguredReportsTrue(t *testing.T) {
	c, _ := New("http://127.0.0.1:1984/")
	if !c.Configured() {
		t.Fatalf("Configured() = false, want true for a constructed Client")
	}
	if u := Unconfigured(); u.Configured() {
		t.Fatalf("Unconfigured().Configured() = true, want false")
	}
}

// List talks to the stream-server's GET /api/profiles endpoint
// which returns a JSON array of profile objects with snake_case
// keys (the 11-field schema on Profile). The client decodes them
// directly into []Profile and sorts by Name for stable admin-UI
// rendering.
func TestListDecodesArrayShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/profiles" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"mjpeg_bal","camera_id":"cam-1","quality":"low","usage":"esp","description":"","codec":"mjpeg","width":800,"height":1280,"fps":9,"encode_quality":6,"encryption":"tls"},
			{"name":"intercom_web","camera_id":"cam-1","quality":"high","usage":"browser","description":"","codec":"h264_passthrough","width":0,"height":0,"fps":0,"encode_quality":0,"encryption":"srtp"}
		]`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	profiles, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("want 2 profiles, got %d", len(profiles))
	}
	// Sorted alphabetically by Name.
	if profiles[0].Name != "intercom_web" || profiles[1].Name != "mjpeg_bal" {
		t.Fatalf("unexpected order: %+v", profiles)
	}
	if profiles[0].Codec != "h264_passthrough" {
		t.Errorf("intercom_web codec = %q, want h264_passthrough", profiles[0].Codec)
	}
	if profiles[0].Usage != "browser" {
		t.Errorf("intercom_web usage = %q, want browser", profiles[0].Usage)
	}
	if profiles[0].Encryption != "srtp" {
		t.Errorf("intercom_web encryption = %q, want srtp", profiles[0].Encryption)
	}
	if profiles[1].Width != 800 || profiles[1].Height != 1280 || profiles[1].FPS != 9 {
		t.Errorf("mjpeg_bal dims = %dx%d @%d, want 800x1280 @9",
			profiles[1].Width, profiles[1].Height, profiles[1].FPS)
	}
	if profiles[1].EncodeQuality != 6 {
		t.Errorf("mjpeg_bal encode_quality = %d, want 6", profiles[1].EncodeQuality)
	}
	if profiles[1].CameraID != "cam-1" {
		t.Errorf("mjpeg_bal camera_id = %q, want cam-1", profiles[1].CameraID)
	}
	if profiles[1].Encryption != "tls" {
		t.Errorf("mjpeg_bal encryption = %q, want tls", profiles[1].Encryption)
	}
}

// Put sends the full 11-field snake_case envelope to the
// stream-server. The test pins the path (PathEscape on the
// profile name), the method, the Content-Type header, and the
// JSON body shape so a future cross-language refactor cannot
// silently drop a field.
func TestPutSendsSnakeCaseBody(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotCT     string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	auto := 6
	err := c.Put(context.Background(), Profile{
		Name:          "intercom esp",
		CameraID:      "cam-1",
		Quality:       "low",
		Usage:         "esp",
		Description:   "ESP-Pull",
		Codec:         "mjpeg",
		Width:         800,
		Height:        1280,
		FPS:           9,
		EncodeQuality: auto,
		Encryption:    "srtp",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	// r.URL.Path is the decoded form; what matters is the
	// PathEscape round-trip, which Go's server side normalises
	// back to a space.
	if gotPath != "/api/profiles/intercom esp" {
		t.Errorf("path = %q, want decoded form of PathEscape(name)", gotPath)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	// Exactly the 11 snake_case keys, no extras.
	wantKeys := []string{
		"name", "camera_id", "quality", "usage", "description",
		"codec", "width", "height", "fps", "encode_quality",
		"encryption",
	}
	if len(gotBody) != len(wantKeys) {
		t.Errorf("body has %d keys, want %d (%v)", len(gotBody), len(wantKeys), gotBody)
	}
	for _, k := range wantKeys {
		if _, ok := gotBody[k]; !ok {
			t.Errorf("body missing key %q", k)
		}
	}
	if gotBody["encryption"] != "srtp" {
		t.Errorf("encryption = %v, want srtp", gotBody["encryption"])
	}
	if v, _ := gotBody["encode_quality"].(float64); v != 6 {
		t.Errorf("encode_quality = %v, want 6", gotBody["encode_quality"])
	}
}

// Put surfaces 400-validation errors verbatim so the admin UI
// can show the operator what the stream-server rejected.
func TestPutSurfacesValidationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "encryption must be tls or srtp", http.StatusBadRequest)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	err := c.Put(context.Background(), Profile{Name: "x", Encryption: "garbage"})
	if err == nil {
		t.Fatal("Put: want error on HTTP 400, got nil")
	}
	if !strings.Contains(err.Error(), "encryption must be tls or srtp") {
		t.Errorf("Put: error %q does not carry server reason", err)
	}
}

// Put rejects an empty name locally without touching the
// backend.
func TestPutRejectsEmptyName(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	if err := c.Put(context.Background(), Profile{Name: "   "}); err == nil {
		t.Fatal("want error on empty name, got nil")
	}
	if hit {
		t.Error("backend was hit; empty-name guard must be local")
	}
}

// Get walks the list response (the stream-server has no single-
// profile GET endpoint) and returns ErrProfileNotFound when the
// name is missing. The test asserts both the missing-case and a
// success case so a future "I'll just route Get to a single-GET
// again" temptation cannot slip past.
func TestGetWalksListAndMapsNotFound(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/api/profiles" {
			t.Errorf("unexpected single-profile path %q; Get must go through /api/profiles", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"mjpeg_bal","camera_id":"cam-1","quality":"low","usage":"esp","description":"","codec":"mjpeg","width":800,"height":1280,"fps":9,"encode_quality":6,"encryption":"tls"}
		]`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL)

	// Hit: returns the matching profile.
	p, err := c.Get(context.Background(), "mjpeg_bal")
	if err != nil {
		t.Fatalf("Get(mjpeg_bal): %v", err)
	}
	if p.Name != "mjpeg_bal" || p.Encryption != "tls" {
		t.Errorf("Get returned unexpected profile: %+v", p)
	}

	// Miss: ErrProfileNotFound.
	if _, err := c.Get(context.Background(), "ghost"); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("Get(ghost): want ErrProfileNotFound, got %v", err)
	}
	if hits != 2 {
		t.Errorf("backend hits = %d, want 2 (one per Get)", hits)
	}
}

func TestDeleteRequestsBackend(t *testing.T) {
	var calledMethod, calledPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledMethod = r.Method
		calledPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	if err := c.Delete(context.Background(), "intercom_high"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if calledMethod != http.MethodDelete {
		t.Errorf("delete method = %q, want DELETE", calledMethod)
	}
	want := "/api/profiles/intercom_high"
	if calledPath != want {
		t.Fatalf("delete path: want %q got %q", want, calledPath)
	}
}

// The transitional client has no Protect connection of its own;
// ListCameras returns an empty slice so the admin UI's
// camera-dropdown can render an "Quelle waehlbar ab
// Stream-Server"-Hinweis.
func TestListCamerasIsEmpty(t *testing.T) {
	c, _ := New("http://127.0.0.1:1984/")
	cams, err := c.ListCameras(context.Background())
	if err != nil {
		t.Fatalf("ListCameras: %v", err)
	}
	if len(cams) != 0 {
		t.Errorf("ListCameras returned %d entries, want 0", len(cams))
	}
}

// backend.go's unconfiguredBackend covers the public-build
// default. Verify the empty-URL + ErrNotConfigured surface so
// main.go can wire it without nil-checks.
func TestUnconfiguredBackend(t *testing.T) {
	b := Unconfigured()
	if b.MJPEGURL("x") != "" {
		t.Errorf("MJPEGURL: want empty, got %q", b.MJPEGURL("x"))
	}
	if b.WebRTCSignalURL("x") != "" {
		t.Errorf("WebRTCSignalURL: want empty, got %q", b.WebRTCSignalURL("x"))
	}
	if _, err := b.List(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("List: want ErrNotConfigured, got %v", err)
	}
	if _, err := b.Get(context.Background(), "x"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Get: want ErrNotConfigured, got %v", err)
	}
	if err := b.Put(context.Background(), Profile{Name: "x"}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Put: want ErrNotConfigured, got %v", err)
	}
	if err := b.Delete(context.Background(), "x"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Delete: want ErrNotConfigured, got %v", err)
	}
	cams, err := b.ListCameras(context.Background())
	if err != nil {
		t.Errorf("ListCameras: %v", err)
	}
	if len(cams) != 0 {
		t.Errorf("ListCameras: want 0 entries, got %d", len(cams))
	}
}
