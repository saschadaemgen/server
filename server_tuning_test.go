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

// TestProfilePut_AcceptsValidEncryption — S6-12 happy path for the new
// field. Tls/srtp/empty all go through; the persisted profile carries
// the right value.
func TestProfilePut_AcceptsValidEncryption(t *testing.T) {
	for _, enc := range []string{"", "tls", "srtp"} {
		t.Run("enc="+enc, func(t *testing.T) {
			fw := &fakeWriter{}
			srv := freshServer(t, ServerOptions{ProfileWriter: fw})
			body := strings.NewReader(`{
				"camera_id":"abc","quality":"high","usage":"esp",
				"codec":"mjpeg","width":800,"height":1280,"fps":12,"encode_quality":6,
				"encryption":"` + enc + `"
			}`)
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/api/profiles/x", body)
			req.SetPathValue("name", "x")
			srv.handleProfilePut(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("encryption=%q: status=%d body=%s", enc, rr.Code, rr.Body.String())
			}
			if len(fw.puts) != 1 {
				t.Fatalf("PutProfile called %d times", len(fw.puts))
			}
			if got := string(fw.puts[0].Encryption); got != enc {
				t.Errorf("persisted Encryption = %q, want %q", got, enc)
			}
		})
	}
}

// TestProfilePut_RejectsInvalidEncryption — anything outside {tls,srtp,""}
// gets a 400 from profile.Validate.
func TestProfilePut_RejectsInvalidEncryption(t *testing.T) {
	fw := &fakeWriter{}
	srv := freshServer(t, ServerOptions{ProfileWriter: fw})
	body := strings.NewReader(`{
		"camera_id":"abc","quality":"high","usage":"esp",
		"codec":"mjpeg","width":800,"height":1280,"fps":12,"encode_quality":6,
		"encryption":"wireguard"
	}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/profiles/x", body)
	req.SetPathValue("name", "x")
	srv.handleProfilePut(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (invalid encryption); body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Encryption") {
		t.Errorf("error body should mention Encryption: %s", rr.Body.String())
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

// TestSourceKeyFor_IgnoresProfileEncryption is the S6-14 inverse of
// the (now-removed) S6-12 TestSourceKeyFor_EncryptionDistinct. The
// camera-side encryption mode is now SERVER-GLOBAL — two profiles on
// the same (camera, quality) MUST get the same Key regardless of
// their stored per-profile encryption value, otherwise the fan-out
// invariant degrades and legacy admin clients that PUT
// encryption="srtp" by accident would silently spin up a second
// (failing) ffmpeg pull.
func TestSourceKeyFor_IgnoresProfileEncryption(t *testing.T) {
	srv := freshServer(t, ServerOptions{Encryption: profile.EncryptionTLS})

	p := profile.Profile{
		Name: "x", CameraID: "abc", Quality: profile.QualityHigh,
		Usage: profile.UsageBrowser, Codec: profile.CodecH264Passthrough,
	}
	p.Encryption = profile.EncryptionTLS
	a := srv.sourceKeyFor(p)
	p.Encryption = profile.EncryptionSRTP
	b := srv.sourceKeyFor(p)
	if a != b {
		t.Errorf("S6-14 contract: per-profile Encryption must NOT affect Key. got tls→%v, srtp→%v", a, b)
	}
}

// TestSourceKeyFor_UsesServerGlobal is the positive half — the
// global mode (set via ServerOptions.Encryption) DOES make it into
// the Key, so flipping it between two distinct server instances
// produces distinct hubs.
func TestSourceKeyFor_UsesServerGlobal(t *testing.T) {
	p := profile.Profile{
		Name: "x", CameraID: "abc", Quality: profile.QualityHigh,
		Usage: profile.UsageBrowser, Codec: profile.CodecH264Passthrough,
	}

	tlsSrv := freshServer(t, ServerOptions{Encryption: profile.EncryptionTLS})
	srtpSrv := freshServer(t, ServerOptions{Encryption: profile.EncryptionSRTP})

	tlsKey := tlsSrv.sourceKeyFor(p)
	srtpKey := srtpSrv.sourceKeyFor(p)

	if tlsKey.Encryption != "tls" {
		t.Errorf("tls-configured server: Key.Encryption=%q, want 'tls'", tlsKey.Encryption)
	}
	if srtpKey.Encryption != "srtp" {
		t.Errorf("srtp-configured server: Key.Encryption=%q, want 'srtp'", srtpKey.Encryption)
	}
	if tlsKey == srtpKey {
		t.Errorf("global mode must distinguish Keys: tls=%v srtp=%v", tlsKey, srtpKey)
	}
}

// TestSourceKeyFor_EmptyServerEncryptionDefaultsToTLS — leaving the
// ServerOptions.Encryption field unset (the default zero value)
// must canonicalise to "tls" in the Key, so admin clients that
// don't set the option still get a sensible default.
func TestSourceKeyFor_EmptyServerEncryptionDefaultsToTLS(t *testing.T) {
	srv := freshServer(t, ServerOptions{}) // Encryption left blank
	p := profile.Profile{
		Name: "x", CameraID: "abc", Quality: profile.QualityHigh,
		Usage: profile.UsageBrowser, Codec: profile.CodecH264Passthrough,
	}
	k := srv.sourceKeyFor(p)
	if k.Encryption != "tls" {
		t.Errorf("default server Key.Encryption=%q, want 'tls'", k.Encryption)
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
// list → edit one field → save.
//
// S6-14 nuance: the GET output's `encryption` field is the SERVER-GLOBAL
// mode, not the per-profile stored value. The round-trip is still
// "GET → PUT" byte-exact on the wire, but the PERSISTED encryption
// after the round-trip is the global value (it's what GET wrote back
// into the PUT body). For non-encryption fields the round-trip
// preserves the seed value as before.
func TestProfiles_GetPut_RoundTrip(t *testing.T) {
	// Seed one realistic profile (MJPEG, all encode-params populated).
	// The per-profile Encryption is deliberately srtp here to exercise
	// the S6-14 "global overrides profile" behaviour: GET will read
	// "tls" (the freshServer default), and the round-trip will persist
	// "tls" back.
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
		Encryption:    profile.EncryptionSRTP,
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

	// Sanity: the field names MUST be snake_case (S6-09 contract +
	// S6-12 encryption).
	for _, want := range []string{"name", "camera_id", "quality", "usage", "description", "codec", "width", "height", "fps", "encode_quality", "encryption"} {
		if _, ok := entry[want]; !ok {
			t.Errorf("GET entry missing snake_case field %q; got: %v", want, entry)
		}
	}
	// S6-14: GET reflects the SERVER-GLOBAL encryption, not the seeded
	// profile.Encryption=srtp. freshServer leaves ServerOptions.Encryption
	// empty → server canonicalises to "tls" → GET shows "tls".
	if got := entry["encryption"]; got != "tls" {
		t.Errorf("GET encryption = %v, want %q (S6-14: server-global wins over per-profile seed)", got, "tls")
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

	// Step 4: PUT persists the body's encryption value verbatim. Since
	// GET wrote the server-global "tls" into the body, the persisted
	// Encryption is "tls", NOT the seed's "srtp". This is the S6-14
	// behaviour — the wire shape carries the live mode, and a naive
	// GET→PUT loop snaps the stored value to that live mode.
	if len(fw.puts) != 1 {
		t.Fatalf("PutProfile called %d times, want 1", len(fw.puts))
	}
	got := fw.puts[0]
	wantPersisted := seed
	wantPersisted.Encryption = profile.EncryptionTLS // S6-14: round-trip snaps to global
	if got != wantPersisted {
		t.Errorf("round-trip data mismatch:\ngot:  %+v\nwant: %+v", got, wantPersisted)
	}
}
