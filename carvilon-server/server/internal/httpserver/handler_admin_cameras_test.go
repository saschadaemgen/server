package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"carvilon.local/server/internal/streams"
)

// camerasBackend is a minimal streams.StreamBackend that lets the
// cameras.json test control the camera list, the error, and the
// Configured() verdict. Only ListCameras / Configured are exercised by
// the handler; the rest return the not-configured sentinel.
type camerasBackend struct {
	cams       []streams.Camera
	err        error
	configured bool
}

func (camerasBackend) MJPEGURL(string) string        { return "" }
func (camerasBackend) WebRTCSignalURL(string) string { return "" }
func (b camerasBackend) Configured() bool            { return b.configured }
func (camerasBackend) List(context.Context) ([]streams.Profile, error) {
	return nil, streams.ErrNotConfigured
}
func (camerasBackend) Get(context.Context, string) (streams.Profile, error) {
	return streams.Profile{}, streams.ErrNotConfigured
}
func (camerasBackend) Put(context.Context, streams.Profile) error { return streams.ErrNotConfigured }
func (camerasBackend) Delete(context.Context, string) error       { return streams.ErrNotConfigured }
func (b camerasBackend) ListCameras(context.Context) ([]streams.Camera, error) {
	return b.cams, b.err
}
func (camerasBackend) Stats(context.Context) (map[string]streams.ProfileStats, error) {
	return nil, streams.ErrNotConfigured
}

// TestAdminCamerasJSON_ListsCameras proves /a/cameras.json returns the
// backend camera list in the dropdown shape (id + name + online +
// has_package_cam) - the source for the per-viewer camera-assignment UI
// (S20-E5).
func TestAdminCamerasJSON_ListsCameras(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.srv.streams = camerasBackend{
		configured: true,
		cams: []streams.Camera{
			{ID: "cam-a", Name: "Intercom", Online: true, HasPackageCam: true},
			{ID: "cam-b", Name: "Flur 1", Online: false},
		},
	}

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/a/cameras.json", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /a/cameras.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Configured bool         `json:"configured"`
		Cameras    []cameraJSON `json:"cameras"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Configured {
		t.Error("configured = false, want true")
	}
	if len(body.Cameras) != 2 {
		t.Fatalf("cameras = %+v, want 2", body.Cameras)
	}
	if body.Cameras[0].ID != "cam-a" || body.Cameras[0].Name != "Intercom" {
		t.Errorf("cameras[0] = %+v, want cam-a/Intercom", body.Cameras[0])
	}
	if !body.Cameras[0].Online || !body.Cameras[0].HasPackageCam {
		t.Errorf("cameras[0] flags = %+v, want online+package", body.Cameras[0])
	}
	if body.Cameras[1].Online {
		t.Errorf("cameras[1].Online = true, want false (Flur 1 offline)")
	}
}

// Unconfigured backend (public build / no Protect): 200 with
// configured=false and an empty (non-null) cameras array.
func TestAdminCamerasJSON_UnconfiguredReturnsEmpty(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.srv.streams = streams.Unconfigured()

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/a/cameras.json", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Configured bool         `json:"configured"`
		Cameras    []cameraJSON `json:"cameras"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Configured {
		t.Error("configured = true, want false for the unconfigured backend")
	}
	if body.Cameras == nil || len(body.Cameras) != 0 {
		t.Errorf("cameras = %+v, want empty non-null array", body.Cameras)
	}
}

// A backend error degrades to 502 (not a 200 with a half list).
func TestAdminCamerasJSON_BackendErrorIs502(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.srv.streams = camerasBackend{configured: true, err: errors.New("protect unreachable")}

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/a/cameras.json", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

// TestAdminCamerasJSON_RequiresAdmin: no admin session -> not 200.
func TestAdminCamerasJSON_RequiresAdmin(t *testing.T) {
	env := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/a/cameras.json", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = 200 without admin session, want redirect/401")
	}
}
