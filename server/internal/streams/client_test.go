package streams

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// Saison 15-01: the seam reserved /offer?src=<profile> for the
// browser WebRTC signalling POST. WebRTCSignalURL must build the
// same path against the backend so the proxy handler can copy the
// body straight through.
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

func TestListDecodesGo2RTCShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/streams" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"intercom_esp": {"producers":[{"url":"rtsps://example/x"}],"consumers":[{"id":1},{"id":2}]},
			"intercom_browser": {"producers":[{"url":"ffmpeg:src#video=mjpeg"}],"consumers":[]}
		}`))
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
	if profiles[0].Name != "intercom_browser" || profiles[1].Name != "intercom_esp" {
		t.Fatalf("unexpected order: %+v", profiles)
	}
	if profiles[1].Consumers != 2 {
		t.Fatalf("intercom_esp consumers: want 2, got %d", profiles[1].Consumers)
	}
	// Saison 15-01: structured fields stay empty for the
	// transitional go2rtc backend.
	if profiles[0].CameraID != "" || profiles[0].Quality != "" || profiles[0].Usage != "" {
		t.Errorf("transitional Profile should leave structured fields empty, got %+v", profiles[0])
	}
}

// Saison 15-01: Put returns ErrNotConfigured because profile CRUD
// is migrating to the carvilon-streaming-server. The admin UI
// flashes the migration message; no go2rtc REST call is made.
func TestPutIsTransitionalStub(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	err := c.Put(context.Background(), Profile{
		Name:        "intercom_esp",
		CameraID:    "cam-1",
		Quality:     "low",
		Usage:       "esp",
		Description: "ESP profile",
	})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Put: want ErrNotConfigured, got %v", err)
	}
	if hit {
		t.Error("Put hit go2rtc REST; should be a local stub")
	}
}

func TestGetMapsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	_, err := c.Get(context.Background(), "ghost")
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("want ErrProfileNotFound, got %v", err)
	}
}

func TestDeleteRequestsBackend(t *testing.T) {
	var calledPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPath = r.URL.Path + "?" + r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	if err := c.Delete(context.Background(), "intercom_high"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := "/api/streams?src=intercom_high"
	if calledPath != want {
		t.Fatalf("delete path: want %q got %q", want, calledPath)
	}
}

// Saison 15-01: go2rtc has no Protect connection; ListCameras
// returns an empty slice so the admin UI's camera-dropdown can
// render an "Quelle waehlbar ab Stream-Server"-Hinweis.
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

// Saison 15-01: backend.go's unconfiguredBackend covers the
// public-build default. Verify the empty-URL + ErrNotConfigured
// surface so main.go can wire it without nil-checks.
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
