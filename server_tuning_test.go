package stream

import (
	"context"
	"encoding/json"
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

func TestProfilePut_URLNameWinsOverBodyName(t *testing.T) {
	// S6-09: `name` in the body is tolerated (a GET response carries
	// it, the briefing wants the GET→PUT round-trip to work without
	// stripping). The URL path remains authoritative; the body's
	// `name` is silently ignored.
	//
	// Before S6-09 this test asserted the opposite — 400 from
	// DisallowUnknownFields. Now we assert (1) the request succeeds
	// and (2) the persisted profile carries the URL's name, not the
	// body's.
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

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body name tolerated); body=%s", rr.Code, rr.Body.String())
	}
	if len(fw.puts) != 1 {
		t.Fatalf("PutProfile called %d times, want 1", len(fw.puts))
	}
	if got := fw.puts[0].Name; got != "mjpeg_bal" {
		t.Errorf("persisted Name = %q, want %q (URL wins over body)", got, "mjpeg_bal")
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

// TestProfiles_GetPut_RoundTrip is the S6-09 contract — a profile object
// fetched via GET /api/profiles can be POSTed back via
// PUT /api/profiles/{name} verbatim, with no field rename and no
// dropping of keys. That's the carvilon-admin's day-to-day workflow:
// list → edit one field → save. Before S6-09 the GET output carried
// `cameraID` / `encodeQuality` (camelCase) while PUT expected
// `camera_id` / `encode_quality` (snake_case); the round-trip
// failed at PUT with 400. Now both speak snake_case.
func TestProfiles_GetPut_RoundTrip(t *testing.T) {
	// Seed one realistic profile (MJPEG, all encode-params populated).
	seed := profile.Profile{
		Name:          "mjpeg_bal",
		CameraID:      "679573e101080b03e4000424",
		Quality:       profile.QualityHigh,
		Usage:         profile.UsageESP,
		Description:   "ESP: MJPEG, 800x1280 @ 12 fps, q:v 6 (balanced)",
		Codec:         profile.CodecMJPEG,
		Width:         800,
		Height:        1280,
		FPS:           12,
		EncodeQuality: 6,
	}
	reg, err := profile.NewRegistry([]profile.Profile{seed})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fw := &fakeWriter{}
	srv := freshServer(t, ServerOptions{Profiles: reg, ProfileWriter: fw})

	// Step 1: GET /api/profiles.
	rrGet := httptest.NewRecorder()
	srv.handleProfiles(rrGet, httptest.NewRequest(http.MethodGet, "/api/profiles", nil))
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rrGet.Code)
	}

	// Step 2: parse the JSON array, take the first entry.
	var list []map[string]any
	if err := json.Unmarshal(rrGet.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode GET body: %v; body=%s", err, rrGet.Body.String())
	}
	if len(list) != 1 {
		t.Fatalf("GET returned %d entries, want 1", len(list))
	}
	entry := list[0]

	// Sanity: the field names MUST be snake_case (S6-09 contract).
	for _, want := range []string{"name", "camera_id", "quality", "usage", "description", "codec", "width", "height", "fps", "encode_quality"} {
		if _, ok := entry[want]; !ok {
			t.Errorf("GET entry missing snake_case field %q; got: %v", want, entry)
		}
	}
	for _, forbidden := range []string{"cameraID", "encodeQuality"} {
		if _, ok := entry[forbidden]; ok {
			t.Errorf("GET entry still carries camelCase field %q — S6-09 regression", forbidden)
		}
	}

	// Step 3: send the entry back via PUT, verbatim. No rename, no
	// stripping of `name`. URL identifies the profile.
	putBody, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal PUT body: %v", err)
	}
	rrPut := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/profiles/mjpeg_bal", strings.NewReader(string(putBody)))
	req.SetPathValue("name", "mjpeg_bal")
	srv.handleProfilePut(rrPut, req)

	if rrPut.Code != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204; body=%s\nPUT-body=%s", rrPut.Code, rrPut.Body.String(), string(putBody))
	}

	// Step 4: the persisted profile must match the original
	// (proves PUT decoded every field correctly through the
	// snake_case profileJSON).
	if len(fw.puts) != 1 {
		t.Fatalf("PutProfile called %d times, want 1", len(fw.puts))
	}
	got := fw.puts[0]
	if got != seed {
		t.Errorf("round-trip lost data:\ngot:  %+v\nwant: %+v", got, seed)
	}
}
