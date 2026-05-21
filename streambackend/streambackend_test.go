package streambackend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/sourcereg"
	"carvilon.local/stream/internal/store"
	"carvilon.local/stream/internal/unifiapi"
)

// --- test plumbing -----------------------------------------------------------

func freshBackend(t *testing.T, cams *unifiapi.Client) *Backend {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	reg, err := profile.NewRegistry(nil)
	if err != nil {
		t.Fatalf("profile.NewRegistry: %v", err)
	}

	srcReg := sourcereg.New(
		func(sourcereg.Key) (source.VideoSource, error) {
			t.Fatalf("source factory should not be called from backend tests")
			return nil, nil
		},
		log.New(io.Discard, "", 0),
	)
	t.Cleanup(func() { _ = srcReg.Close() })

	b, err := New(Options{
		Store:    s,
		Profiles: reg,
		Sources:  srcReg,
		Cameras:  cams,
		BaseURL:  "http://127.0.0.1:8555",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func wireProfile(name string) Profile {
	return Profile{
		Name:        name,
		CameraID:    "cam-" + name,
		Quality:     "high",
		Usage:       "browser",
		Description: "test " + name,
		// S6-01: browser profiles default to h264_passthrough; no encode
		// params required.
		Codec: "h264_passthrough",
	}
}

// wireMJPEGProfile is the transcoded-codec test fixture: usage=esp with
// MJPEG encode params populated.
func wireMJPEGProfile(name string) Profile {
	return Profile{
		Name:          name,
		CameraID:      "cam-" + name,
		Quality:       "high",
		Usage:         "esp",
		Description:   "test " + name,
		Codec:         "mjpeg",
		Width:         800,
		Height:        1280,
		FPS:           12,
		EncodeQuality: 6,
	}
}

// --- URL builders ------------------------------------------------------------

func TestMJPEGURL(t *testing.T) {
	b := freshBackend(t, nil)
	got := b.MJPEGURL("intercom_esp")
	want := "http://127.0.0.1:8555/api/stream.mjpeg?src=intercom_esp"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMJPEGURL_EscapesQuery(t *testing.T) {
	b := freshBackend(t, nil)
	got := b.MJPEGURL("name with spaces & punct/?")
	if !strings.HasPrefix(got, "http://127.0.0.1:8555/api/stream.mjpeg?src=") {
		t.Errorf("unexpected prefix: %s", got)
	}
	if strings.Contains(got, " ") {
		t.Errorf("URL contains literal space: %s", got)
	}
	if strings.Contains(got, "?src=name with") {
		t.Errorf("URL not query-escaped: %s", got)
	}
}

func TestMJPEGURL_EmptyReturnsEmpty(t *testing.T) {
	b := freshBackend(t, nil)
	if got := b.MJPEGURL(""); got != "" {
		t.Errorf("got %q, want empty for empty profile name", got)
	}
}

func TestWebRTCSignalURL(t *testing.T) {
	b := freshBackend(t, nil)
	got := b.WebRTCSignalURL("intercom_browser")
	want := "http://127.0.0.1:8555/offer?src=intercom_browser"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- CRUD --------------------------------------------------------------------

func TestPutGetRoundTrip(t *testing.T) {
	b := freshBackend(t, nil)
	ctx := context.Background()

	in := wireProfile("intercom_browser")
	if err := b.Put(ctx, in); err != nil {
		t.Fatalf("Put: %v", err)
	}

	out, err := b.Get(ctx, in.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Consumers is runtime — compare excluding it.
	out.Consumers = 0
	if out != in {
		t.Errorf("round-trip:\ngot:  %+v\nwant: %+v", out, in)
	}
}

func TestGet_UnknownIsErrProfileNotFound(t *testing.T) {
	b := freshBackend(t, nil)
	_, err := b.Get(context.Background(), "missing")
	if !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("err = %v, want ErrProfileNotFound chain", err)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q should mention the missing name", err)
	}
}

func TestPut_RejectsInvalid(t *testing.T) {
	b := freshBackend(t, nil)
	bad := wireProfile("bad")
	bad.Quality = "ultra"
	if err := b.Put(context.Background(), bad); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestPut_UpsertUpdatesRegistry(t *testing.T) {
	b := freshBackend(t, nil)
	ctx := context.Background()

	p := wireProfile("intercom_browser")
	_ = b.Put(ctx, p)

	p.Description = "updated"
	if err := b.Put(ctx, p); err != nil {
		t.Fatalf("Put 2: %v", err)
	}

	got, _ := b.Get(ctx, p.Name)
	if got.Description != "updated" {
		t.Errorf("upsert Description = %q, want %q", got.Description, "updated")
	}

	// Registry should also reflect the new description.
	pReg, err := b.opts.Profiles.Get(p.Name)
	if err != nil {
		t.Fatalf("registry Get: %v", err)
	}
	if pReg.Description != "updated" {
		t.Errorf("registry stale: Description = %q, want %q", pReg.Description, "updated")
	}
}

func TestDelete_RemovesFromBothLayers(t *testing.T) {
	b := freshBackend(t, nil)
	ctx := context.Background()

	// S6-01: an ESP profile uses MJPEG (or H.264-CBP); use the
	// transcoded-codec fixture so the test mirrors a realistic profile.
	p := wireMJPEGProfile("intercom_esp")
	_ = b.Put(ctx, p)

	if err := b.Delete(ctx, p.Name); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := b.Get(ctx, p.Name); !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("Get after Delete err = %v, want ErrProfileNotFound", err)
	}
	if _, err := b.opts.Profiles.Get(p.Name); !errors.Is(err, profile.ErrUnknownProfile) {
		t.Errorf("registry still has profile after Delete: err = %v", err)
	}
}

func TestDelete_UnknownIsErrProfileNotFound(t *testing.T) {
	b := freshBackend(t, nil)
	err := b.Delete(context.Background(), "never-existed")
	if !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("err = %v, want ErrProfileNotFound chain", err)
	}
}

func TestList_SortedAndComplete(t *testing.T) {
	b := freshBackend(t, nil)
	ctx := context.Background()

	for _, name := range []string{"zebra", "alpha", "mike"} {
		p := wireProfile(name)
		if err := b.Put(ctx, p); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}

	list, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"alpha", "mike", "zebra"}
	if len(list) != len(want) {
		t.Fatalf("List len = %d, want %d", len(list), len(want))
	}
	for i, n := range want {
		if list[i].Name != n {
			t.Errorf("List[%d].Name = %q, want %q", i, list[i].Name, n)
		}
	}
}

// --- Configured + URL behaviour ---------------------------------------------

func TestConfigured(t *testing.T) {
	b := freshBackend(t, nil)
	if !b.Configured() {
		t.Error("Configured() = false, want true (real Backend is always configured)")
	}
}

// --- ListCameras -------------------------------------------------------------

func TestListCameras_EmptyWhenNoClient(t *testing.T) {
	b := freshBackend(t, nil)
	cams, err := b.ListCameras(context.Background())
	if err != nil {
		t.Fatalf("ListCameras: %v", err)
	}
	if len(cams) != 0 {
		t.Errorf("got %d cameras, want 0 when no Protect client wired", len(cams))
	}
}

func TestListCameras_ProxiesToProtect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/protect/integration/v1/cameras" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[
			{"id":"abc","name":"Intercom","state":"CONNECTED","hasPackageCamera":true}
		]`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "https://")
	client, err := unifiapi.New(unifiapi.Options{
		NVRHost:    host,
		APIKey:     "k",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("unifiapi.New: %v", err)
	}

	b := freshBackend(t, client)
	cams, err := b.ListCameras(context.Background())
	if err != nil {
		t.Fatalf("ListCameras: %v", err)
	}
	if len(cams) != 1 {
		t.Fatalf("got %d cameras, want 1", len(cams))
	}
	if cams[0].ID != "abc" {
		t.Errorf("ID = %q, want abc", cams[0].ID)
	}
	if !cams[0].Online {
		t.Error("camera should be Online (state=CONNECTED)")
	}
	if !cams[0].HasPackageCam {
		t.Error("camera should HasPackageCam")
	}
}

// --- Seed semantics (the briefing §2 hard rule) -----------------------------

func TestSeedSemantics_DBWinsOverJSON(t *testing.T) {
	// Simulate the briefing's full lifecycle:
	//   1. Empty DB + JSON seed → Seed populates the DB.
	//   2. New process starts with DIFFERENT JSON seed → the existing DB
	//      wins; the new JSON is ignored.
	//   3. New process starts without JSON → DB is loaded as-is.

	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// Step 1: empty DB, JSON seed with one profile.
	seedA := []profile.Profile{{
		Name: "from-seed-A", CameraID: "cam-a",
		Quality: profile.QualityHigh, Usage: profile.UsageBrowser,
		Description: "from first JSON",
		Codec:       profile.CodecH264Passthrough,
	}}
	if n, err := s.SeedIfEmpty(ctx, seedA); err != nil || n != 1 {
		t.Fatalf("first SeedIfEmpty: n=%d err=%v, want (1, nil)", n, err)
	}

	// Step 2: DIFFERENT JSON; DB is already populated → no-op.
	seedB := []profile.Profile{{
		Name: "would-have-been-seeded-B", CameraID: "cam-b",
		Quality: profile.QualityHigh, Usage: profile.UsageBrowser,
		Codec: profile.CodecH264Passthrough,
	}}
	if n, err := s.SeedIfEmpty(ctx, seedB); err != nil || n != 0 {
		t.Fatalf("second SeedIfEmpty: n=%d err=%v, want (0, nil)", n, err)
	}

	// The DB still has only the A-set.
	list, _ := s.List(ctx)
	if len(list) != 1 || list[0].Name != "from-seed-A" {
		t.Errorf("DB should still contain only A; got %v", list)
	}

	// Step 3: build a backend that hydrates from the DB. The registry
	// must end up matching the DB exactly.
	reg, _ := profile.NewRegistry(nil)
	srcReg := sourcereg.New(
		func(sourcereg.Key) (source.VideoSource, error) {
			return nil, errors.New("no source factory in test")
		},
		log.New(io.Discard, "", 0),
	)
	defer srcReg.Close()

	b, err := New(Options{
		Store: s, Profiles: reg, Sources: srcReg, BaseURL: "http://x",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	names := b.opts.Profiles.Names()
	if len(names) != 1 || names[0] != "from-seed-A" {
		t.Errorf("registry Names = %v, want [from-seed-A]", names)
	}
}

// --- New() validation -------------------------------------------------------

func TestNew_RequiresStore(t *testing.T) {
	_, err := New(Options{BaseURL: "http://x"})
	if err == nil {
		t.Error("expected error for missing Store")
	}
}

func TestNew_RequiresProfiles(t *testing.T) {
	s, _ := store.Open(":memory:")
	defer s.Close()
	_, err := New(Options{Store: s, BaseURL: "http://x"})
	if err == nil {
		t.Error("expected error for missing Profiles")
	}
}

func TestNew_RequiresSources(t *testing.T) {
	s, _ := store.Open(":memory:")
	defer s.Close()
	reg, _ := profile.NewRegistry(nil)
	_, err := New(Options{Store: s, Profiles: reg, BaseURL: "http://x"})
	if err == nil {
		t.Error("expected error for missing Sources")
	}
}

func TestNew_RequiresBaseURL(t *testing.T) {
	s, _ := store.Open(":memory:")
	defer s.Close()
	reg, _ := profile.NewRegistry(nil)
	srcReg := sourcereg.New(
		func(sourcereg.Key) (source.VideoSource, error) { return nil, errors.New("nope") },
		log.New(io.Discard, "", 0),
	)
	defer srcReg.Close()
	_, err := New(Options{Store: s, Profiles: reg, Sources: srcReg})
	if err == nil {
		t.Error("expected error for missing BaseURL")
	}
}

// --- Wire-shape parity ------------------------------------------------------

// Lock in the JSON tags so the carvilon-side build-tag wrapper can
// convert field-by-field without surprises.
func TestProfile_WireTags(t *testing.T) {
	in := Profile{
		Name:          "intercom_esp",
		CameraID:      "abc",
		Quality:       "high",
		Usage:         "esp",
		Description:   "desc",
		Consumers:     3,
		Codec:         "mjpeg",
		Width:         800,
		Height:        1280,
		FPS:           12,
		EncodeQuality: 6,
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{
		`"name":"intercom_esp"`,
		`"camera_id":"abc"`,
		`"quality":"high"`,
		`"usage":"esp"`,
		`"description":"desc"`,
		`"consumers":3`,
		`"codec":"mjpeg"`,
		`"width":800`,
		`"height":1280`,
		`"fps":12`,
		`"encode_quality":6`,
	} {
		if !strings.Contains(string(buf), want) {
			t.Errorf("Profile JSON missing %q: %s", want, buf)
		}
	}
}

// TestProfile_RoundTripsTranscodedCodec asserts that a wire-shape MJPEG
// profile survives a CRUD round-trip with all codec fields intact —
// proving the toWire/fromWire mapping covers the new S6 columns.
func TestProfile_RoundTripsTranscodedCodec(t *testing.T) {
	b := freshBackend(t, nil)
	ctx := context.Background()

	in := wireMJPEGProfile("intercom_esp")
	if err := b.Put(ctx, in); err != nil {
		t.Fatalf("Put: %v", err)
	}
	out, err := b.Get(ctx, in.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	out.Consumers = 0
	if out != in {
		t.Errorf("MJPEG profile round-trip:\ngot:  %+v\nwant: %+v", out, in)
	}
}

func TestCamera_WireTags(t *testing.T) {
	in := Camera{ID: "x", Name: "n", Online: true, HasPackageCam: true}
	buf, _ := json.Marshal(in)
	for _, want := range []string{
		`"id":"x"`, `"name":"n"`, `"online":true`, `"has_package_cam":true`,
	} {
		if !strings.Contains(string(buf), want) {
			t.Errorf("Camera JSON missing %q: %s", want, buf)
		}
	}
}
