package stream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"carvilon.local/stream/internal/profile"
)

// fakeWriter records calls so the test asserts the server passed the
// right profile.Profile to the writer.
type fakeWriter struct {
	mu       sync.Mutex
	puts     []profile.Profile
	deletes  []string
	putErr   error
	delErr   error
}

func (f *fakeWriter) PutProfile(_ context.Context, p profile.Profile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, p)
	return f.putErr
}

func (f *fakeWriter) DeleteProfile(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, name)
	return f.delErr
}

func TestProfilePut_Disabled503(t *testing.T) {
	// Server without ProfileWriter must 503 PUT, but the read-only
	// GET /api/profiles still works (we don't test it here, see
	// server_stats_test.go freshServer).
	srv := freshServer(t, ServerOptions{})
	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"camera_id":"c","quality":"high","usage":"esp","codec":"mjpeg","width":800,"height":1280,"fps":12,"encode_quality":6}`)
	req := httptest.NewRequest(http.MethodPut, "/api/profiles/test", body)
	req.SetPathValue("name", "test")
	srv.handleProfilePut(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestProfilePut_HappyPath(t *testing.T) {
	fw := &fakeWriter{}
	srv := freshServer(t, ServerOptions{ProfileWriter: fw})

	rr := httptest.NewRecorder()
	body := strings.NewReader(`{
		"camera_id":"abc","quality":"high","usage":"esp",
		"description":"tuned",
		"codec":"mjpeg","width":800,"height":1280,"fps":15,"encode_quality":5
	}`)
	req := httptest.NewRequest(http.MethodPut, "/api/profiles/mjpeg_bal", body)
	req.SetPathValue("name", "mjpeg_bal")
	srv.handleProfilePut(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	if len(fw.puts) != 1 {
		t.Fatalf("PutProfile called %d times, want 1", len(fw.puts))
	}
	got := fw.puts[0]
	if got.Name != "mjpeg_bal" {
		t.Errorf("Name = %q, want mjpeg_bal (URL must win over body)", got.Name)
	}
	if got.Codec != profile.CodecMJPEG {
		t.Errorf("Codec = %q, want mjpeg", got.Codec)
	}
	if got.Width != 800 || got.Height != 1280 || got.FPS != 15 || got.EncodeQuality != 5 {
		t.Errorf("encode params wrong: %+v", got)
	}
}

func TestProfilePut_URLNameOverridesBodyName(t *testing.T) {
	// If a caller foolishly includes "name" in the body, the URL
	// wins. There's no Name field in our profileJSON so a body Name
	// would actually be rejected by DisallowUnknownFields — assert
	// that path too.
	fw := &fakeWriter{}
	srv := freshServer(t, ServerOptions{ProfileWriter: fw})

	rr := httptest.NewRecorder()
	body := strings.NewReader(`{
		"name":"sneaky",
		"camera_id":"abc","quality":"high","usage":"browser",
		"codec":"h264_passthrough"
	}`)
	req := httptest.NewRequest(http.MethodPut, "/api/profiles/mjpeg_bal", body)
	req.SetPathValue("name", "mjpeg_bal")
	srv.handleProfilePut(rr, req)

	// DisallowUnknownFields → 400.
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (unknown field 'name'); body=%s", rr.Code, rr.Body.String())
	}
}

func TestProfilePut_RejectsInvalidCodec(t *testing.T) {
	fw := &fakeWriter{}
	srv := freshServer(t, ServerOptions{ProfileWriter: fw})

	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"camera_id":"c","quality":"high","usage":"esp","codec":"av1"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/profiles/x", body)
	req.SetPathValue("name", "x")
	srv.handleProfilePut(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (invalid codec); body=%s", rr.Code, rr.Body.String())
	}
	if len(fw.puts) != 0 {
		t.Errorf("PutProfile called with invalid input: %+v", fw.puts)
	}
}

func TestProfilePut_RejectsMissingEncodeParamsForMJPEG(t *testing.T) {
	fw := &fakeWriter{}
	srv := freshServer(t, ServerOptions{ProfileWriter: fw})

	rr := httptest.NewRecorder()
	// codec=mjpeg but width=0 — Validate should reject.
	body := strings.NewReader(`{"camera_id":"c","quality":"high","usage":"esp","codec":"mjpeg"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/profiles/x", body)
	req.SetPathValue("name", "x")
	srv.handleProfilePut(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestProfilePut_PassthroughNoEncodeParamsOK(t *testing.T) {
	fw := &fakeWriter{}
	srv := freshServer(t, ServerOptions{ProfileWriter: fw})

	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"camera_id":"c","quality":"high","usage":"browser","codec":"h264_passthrough"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/profiles/intercom_browser", body)
	req.SetPathValue("name", "intercom_browser")
	srv.handleProfilePut(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestProfileDelete_HappyPath(t *testing.T) {
	fw := &fakeWriter{}
	srv := freshServer(t, ServerOptions{ProfileWriter: fw})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/profiles/mjpeg_bal", nil)
	req.SetPathValue("name", "mjpeg_bal")
	srv.handleProfileDelete(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if len(fw.deletes) != 1 || fw.deletes[0] != "mjpeg_bal" {
		t.Errorf("DeleteProfile = %v, want [mjpeg_bal]", fw.deletes)
	}
}

func TestProfileDelete_Unknown404(t *testing.T) {
	// Writer returns ErrUnknownProfile → handler maps to 404.
	fw := &fakeWriter{delErr: profile.ErrUnknownProfile}
	srv := freshServer(t, ServerOptions{ProfileWriter: fw})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/profiles/nope", nil)
	req.SetPathValue("name", "nope")
	srv.handleProfileDelete(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestProfileDelete_Disabled503(t *testing.T) {
	srv := freshServer(t, ServerOptions{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/profiles/x", nil)
	req.SetPathValue("name", "x")
	srv.handleProfileDelete(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}
