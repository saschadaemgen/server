package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"carvilon.local/stream/internal/profile"
)

func mkProfile(name string) profile.Profile {
	return profile.Profile{
		Name:        name,
		CameraID:    "cam-" + name,
		Quality:     profile.QualityHigh,
		Usage:       profile.UsageBrowser,
		Description: "test " + name,
		// S6-01: h264_passthrough doesn't require encode params, so the
		// default test profile stays terse. Tests that need a transcoded
		// codec use mkMJPEGProfile below.
		Codec: profile.CodecH264Passthrough,
	}
}

// mkMJPEGProfile is the transcoded-codec counterpart to mkProfile, used
// where the codec column matters (round-trip, list, seed).
func mkMJPEGProfile(name string) profile.Profile {
	return profile.Profile{
		Name:          name,
		CameraID:      "cam-" + name,
		Quality:       profile.QualityHigh,
		Usage:         profile.UsageESP,
		Description:   "test " + name,
		Codec:         profile.CodecMJPEG,
		Width:         800,
		Height:        1280,
		FPS:           12,
		EncodeQuality: 6,
	}
}

func openMem(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_Memory(t *testing.T) {
	s := openMem(t)
	if s.Path() != ":memory:" {
		t.Errorf("Path = %q, want :memory:", s.Path())
	}
	n, err := s.Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("fresh DB Count = %d, want 0", n)
	}
}

func TestOpen_FileCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "state", "stream.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open with deep path: %v", err)
	}
	defer s.Close()
	if s.Path() != path {
		t.Errorf("Path = %q, want %q", s.Path(), path)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	in := mkProfile("intercom_browser")
	if err := s.Put(ctx, in); err != nil {
		t.Fatalf("Put: %v", err)
	}
	out, err := s.Get(ctx, "intercom_browser")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\ngot:  %+v\nwant: %+v", out, in)
	}
}

func TestPut_Upsert(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	in := mkProfile("intercom_esp")
	in.Usage = profile.UsageESP
	if err := s.Put(ctx, in); err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	// Same name, different description → upsert
	in2 := in
	in2.Description = "updated"
	if err := s.Put(ctx, in2); err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	out, err := s.Get(ctx, "intercom_esp")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out.Description != "updated" {
		t.Errorf("upsert did not update Description: %+v", out)
	}
	n, err := s.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Errorf("Count after upsert = %d, want 1", n)
	}
}

func TestGet_UnknownReturnsErrNotFound(t *testing.T) {
	s := openMem(t)
	_, err := s.Get(context.Background(), "no-such-profile")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound chain", err)
	}
}

func TestDelete_KnownThenUnknown(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	p := mkProfile("intercom_browser")
	if err := s.Put(ctx, p); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, p.Name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Gone
	if _, err := s.Get(ctx, p.Name); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: Get err = %v, want ErrNotFound", err)
	}
	// Second delete → ErrNotFound
	if err := s.Delete(ctx, p.Name); !errors.Is(err, ErrNotFound) {
		t.Errorf("re-delete: err = %v, want ErrNotFound", err)
	}
}

func TestList_SortedByName(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	for _, n := range []string{"zebra", "alpha", "mike"} {
		if err := s.Put(ctx, mkProfile(n)); err != nil {
			t.Fatalf("Put %s: %v", n, err)
		}
	}
	list, err := s.List(ctx)
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

func TestSeedIfEmpty_FillsEmptyDB(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seed := []profile.Profile{
		mkProfile("intercom_browser"),
		mkProfile("intercom_esp"),
	}
	n, err := s.SeedIfEmpty(ctx, seed)
	if err != nil {
		t.Fatalf("SeedIfEmpty: %v", err)
	}
	if n != 2 {
		t.Errorf("inserted = %d, want 2", n)
	}
	list, _ := s.List(ctx)
	if len(list) != 2 {
		t.Errorf("List len = %d, want 2", len(list))
	}
}

func TestSeedIfEmpty_IsNoOpOnNonEmptyDB(t *testing.T) {
	// The hard rule from the briefing: once the DB has any row, the
	// JSON seed is ignored. Even if it has DIFFERENT profiles.
	s := openMem(t)
	ctx := context.Background()
	// Pre-existing row.
	if err := s.Put(ctx, mkProfile("existing")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Seed with a different set.
	n, err := s.SeedIfEmpty(ctx, []profile.Profile{
		mkProfile("would_have_been_seeded"),
	})
	if err != nil {
		t.Fatalf("SeedIfEmpty: %v", err)
	}
	if n != 0 {
		t.Errorf("inserted = %d, want 0 (DB was non-empty)", n)
	}
	list, _ := s.List(ctx)
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
	if list[0].Name != "existing" {
		t.Errorf("List[0].Name = %q, want %q — seed should NOT have overwritten", list[0].Name, "existing")
	}
}

func TestSeedIfEmpty_NilOrEmptyIsNoOp(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	n, err := s.SeedIfEmpty(ctx, nil)
	if err != nil {
		t.Fatalf("SeedIfEmpty(nil): %v", err)
	}
	if n != 0 {
		t.Errorf("nil seed: inserted = %d, want 0", n)
	}
	n, err = s.SeedIfEmpty(ctx, []profile.Profile{})
	if err != nil {
		t.Fatalf("SeedIfEmpty([]): %v", err)
	}
	if n != 0 {
		t.Errorf("empty seed: inserted = %d, want 0", n)
	}
}

func TestSeedIfEmpty_RejectsInvalidProfile(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	bad := mkProfile("bad")
	bad.Quality = "bogus"
	_, err := s.SeedIfEmpty(ctx, []profile.Profile{bad})
	if err == nil {
		t.Fatal("expected validation error for bad quality")
	}
	n, _ := s.Count(ctx)
	if n != 0 {
		t.Errorf("DB has %d rows after failed seed; want 0 (transaction should roll back)", n)
	}
}

func TestPut_RejectsInvalidProfile(t *testing.T) {
	s := openMem(t)
	bad := mkProfile("bad")
	bad.Quality = "bogus"
	if err := s.Put(context.Background(), bad); err == nil {
		t.Fatal("expected validation error")
	}
}

// TestPutGetRoundTrip_Encryption covers the S6-12 column: a profile
// with encryption=srtp must round-trip byte-identical, including the
// canonical "" → "" preservation (the SourceFactory at the unifi
// boundary normalises "" to "tls"; the store layer doesn't second-
// guess this).
func TestPutGetRoundTrip_Encryption(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	for _, enc := range []profile.Encryption{"", profile.EncryptionTLS, profile.EncryptionSRTP} {
		t.Run(string("enc="+enc), func(t *testing.T) {
			in := mkMJPEGProfile("p_" + string(enc))
			in.Encryption = enc
			if err := s.Put(ctx, in); err != nil {
				t.Fatalf("Put: %v", err)
			}
			out, err := s.Get(ctx, in.Name)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if out.Encryption != enc {
				t.Errorf("Encryption round-trip: got %q, want %q", out.Encryption, enc)
			}
			if out != in {
				t.Errorf("full round-trip mismatch:\ngot:  %+v\nwant: %+v", out, in)
			}
		})
	}
}

// TestPutGetRoundTrip_TranscodedCodec covers the S6-01 columns: a profile
// with codec=mjpeg + width/height/fps/encode_quality must round-trip
// byte-identical through the DB.
func TestPutGetRoundTrip_TranscodedCodec(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	in := mkMJPEGProfile("intercom_esp")
	if err := s.Put(ctx, in); err != nil {
		t.Fatalf("Put: %v", err)
	}
	out, err := s.Get(ctx, in.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\ngot:  %+v\nwant: %+v", out, in)
	}
}

// TestMigration_BackfillsPreS6Rows asserts the upgrade path: if a row
// exists from the S5 schema (no codec column populated, simulated by
// inserting via raw SQL into an already-migrated DB and clearing the
// codec) — actually we just confirm the backfill statements run on
// re-Open and turn a codec='' row into the right codec.
func TestMigration_BackfillsPreS6Rows(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	// Simulate a pre-S6 row by inserting directly with empty codec / zero
	// encode params (bypassing Put which would Validate). This mirrors
	// the state of any row that existed when the columns were ADDed.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO profiles (name, camera_id, quality, usage, description,
		                      codec, width, height, fps, encode_quality)
		VALUES ('legacy_browser', 'cam-x', 'high', 'browser', '', '', 0, 0, 0, 0),
		       ('legacy_esp',     'cam-y', 'high', 'esp',     '', '', 0, 0, 0, 0)
	`); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}

	// Re-run migrations: the UPDATE statements should backfill codec
	// based on usage. This is the same code path Open() runs.
	if err := runMigrations(s.db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	br, err := s.Get(ctx, "legacy_browser")
	if err != nil {
		t.Fatalf("Get legacy_browser: %v", err)
	}
	if br.Codec != profile.CodecH264Passthrough {
		t.Errorf("legacy_browser codec = %q, want %q", br.Codec, profile.CodecH264Passthrough)
	}

	esp, err := s.Get(ctx, "legacy_esp")
	if err != nil {
		t.Fatalf("Get legacy_esp: %v", err)
	}
	if esp.Codec != profile.CodecMJPEG {
		t.Errorf("legacy_esp codec = %q, want %q", esp.Codec, profile.CodecMJPEG)
	}
	if esp.Width != 800 || esp.Height != 1280 || esp.FPS != 12 || esp.EncodeQuality != 6 {
		t.Errorf("legacy_esp encode params not backfilled: %+v", esp)
	}
}

// TestMigration_Idempotent ensures running Open twice in the same
// process — and therefore the migrations twice — is a no-op the second
// time. Specifically the ALTER TABLE statements must not surface the
// "duplicate column" SQLite error.
func TestMigration_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idem.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2 (migrations should be idempotent): %v", err)
	}
	_ = s2.Close()
}

func TestPersistenceAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.db")
	// First instance writes.
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if err := s1.Put(context.Background(), mkProfile("intercom_browser")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Second instance reads.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer s2.Close()
	p, err := s2.Get(context.Background(), "intercom_browser")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if p.Name != "intercom_browser" {
		t.Errorf("got name %q after reopen", p.Name)
	}
}
