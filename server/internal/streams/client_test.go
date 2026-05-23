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
// which returns a JSON array of profile objects with camelCase
// keys (name, cameraID, quality, usage, description, codec,
// width, height, fps, encodeQuality, consumers). The client
// decodes them directly into []Profile and sorts by Name for
// stable admin-UI rendering.
func TestListDecodesArrayShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/profiles" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"mjpeg_bal","cameraID":"cam-1","codec":"mjpeg","width":800,"height":1280,"fps":9,"encodeQuality":6,"consumers":0},
			{"name":"intercom_web","cameraID":"cam-1","codec":"h264_passthrough","consumers":2,"usage":"webrtc"}
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
	if profiles[0].Consumers != 2 {
		t.Errorf("intercom_web consumers = %d, want 2", profiles[0].Consumers)
	}
	if profiles[0].Usage != "webrtc" {
		t.Errorf("intercom_web usage = %q, want webrtc", profiles[0].Usage)
	}
	if profiles[1].Width != 800 || profiles[1].Height != 1280 || profiles[1].FPS != 9 {
		t.Errorf("mjpeg_bal dims = %dx%d @%d, want 800x1280 @9",
			profiles[1].Width, profiles[1].Height, profiles[1].FPS)
	}
	if profiles[1].EncodeQuality != 6 {
		t.Errorf("mjpeg_bal encodeQuality = %d, want 6", profiles[1].EncodeQuality)
	}
	if profiles[1].CameraID != "cam-1" {
		t.Errorf("mjpeg_bal cameraID = %q, want cam-1", profiles[1].CameraID)
	}
}

// Put stays a stub while the stream-server's GET/PUT field-name
// casing is being unified. The admin UI does not call it; if a
// caller invokes Put it must NOT reach the backend.
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
		t.Error("Put hit the backend; should be a local stub")
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
