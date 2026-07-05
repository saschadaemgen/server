package readerstore

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
)

// clock is a controllable test clock so timestamps are deterministic and
// first_seen/last_seen invariants can be asserted precisely.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestStore opens a real temp-file DB so the full migration stack
// (including the 037 readers table) runs.
func newTestStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	c := &clock{t: time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)}
	return New(d.DB, WithClock(c.now)), c
}

func readerA() Detected {
	return Detected{ID: "nfc:i2c-1", Kind: "nfc", Model: "PN532", Firmware: "1.6", Bus: "i2c-1", Name: "RPi-NFC-PN532 (I2C-1)"}
}
func readerB() Detected {
	return Detected{ID: "nfc:i2c-2", Kind: "nfc", Model: "PN532", Firmware: "1.6", Bus: "i2c-2", Name: "RPi-NFC-PN532 (I2C-2)"}
}

func byID(readers []Reader, id string) *Reader {
	for i := range readers {
		if readers[i].ID == id {
			return &readers[i]
		}
	}
	return nil
}

// TestSync_AutoRegistersOnline pins Teil A's auto-registration: a
// simulated detection creates an online row with the speaking auto-name.
func TestSync_AutoRegistersOnline(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	if err := s.Sync(ctx, []Detected{readerA()}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("readers = %d, want 1", len(list))
	}
	r := list[0]
	if r.ID != "nfc:i2c-1" || !r.Online || r.Name != "RPi-NFC-PN532 (I2C-1)" || r.Bus != "i2c-1" {
		t.Fatalf("row = %+v", r)
	}
	if r.DisplayName() != r.Name {
		t.Fatalf("display name = %q, want the auto-name %q", r.DisplayName(), r.Name)
	}
	if r.FirstSeenAt == 0 {
		t.Fatal("first_seen_at not set")
	}
}

// TestSync_StableIdentityNoDuplicate pins the mandatory "second start =
// no duplicate": the same hardware re-detected reuses the row and keeps
// its first-seen time.
func TestSync_StableIdentityNoDuplicate(t *testing.T) {
	ctx := context.Background()
	s, c := newTestStore(t)
	if err := s.Sync(ctx, []Detected{readerA()}); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	first, _ := s.List(ctx)
	firstSeen := first[0].FirstSeenAt

	c.advance(time.Hour) // a later restart
	if err := s.Sync(ctx, []Detected{readerA()}); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	second, _ := s.List(ctx)
	if len(second) != 1 {
		t.Fatalf("after restart readers = %d, want 1 (no duplicate)", len(second))
	}
	if second[0].FirstSeenAt != firstSeen {
		t.Fatalf("first_seen_at moved on restart: %d -> %d", firstSeen, second[0].FirstSeenAt)
	}
}

// TestSync_HardwareGoneStaysOffline pins Teil A's offline rule.
func TestSync_HardwareGoneStaysOffline(t *testing.T) {
	ctx := context.Background()
	s, c := newTestStore(t)
	if err := s.Sync(ctx, []Detected{readerA()}); err != nil {
		t.Fatalf("Sync online: %v", err)
	}
	if err := s.NoteTag(ctx, "nfc:i2c-1", "D6:45:90:3B"); err != nil {
		t.Fatalf("NoteTag: %v", err)
	}

	c.advance(time.Minute)
	if err := s.Sync(ctx, nil); err != nil {
		t.Fatalf("Sync empty: %v", err)
	}
	gone, _ := s.List(ctx)
	if len(gone) != 1 {
		t.Fatalf("offline reader deleted (rows=%d), want kept", len(gone))
	}
	if gone[0].Online {
		t.Fatal("missing reader still online")
	}
	if gone[0].LastUID != "D6:45:90:3B" || gone[0].LastSeenAt == 0 {
		t.Fatalf("offline reader lost its last-seen tag: %+v", gone[0])
	}

	c.advance(time.Minute)
	if err := s.Sync(ctx, []Detected{readerA()}); err != nil {
		t.Fatalf("Sync back online: %v", err)
	}
	back, _ := s.List(ctx)
	if len(back) != 1 || !back[0].Online {
		t.Fatalf("reader did not come back online: %+v", back)
	}
}

// TestSync_PartialSetOnlyDetectedOnline pins that a mixed sync flips
// exactly the absent readers offline.
func TestSync_PartialSetOnlyDetectedOnline(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	if err := s.Sync(ctx, []Detected{readerA(), readerB()}); err != nil {
		t.Fatalf("Sync both: %v", err)
	}
	if err := s.Sync(ctx, []Detected{readerA()}); err != nil {
		t.Fatalf("Sync only A: %v", err)
	}
	list, _ := s.List(ctx)
	a, b := byID(list, "nfc:i2c-1"), byID(list, "nfc:i2c-2")
	if a == nil || !a.Online {
		t.Fatalf("A should be online: %+v", a)
	}
	if b == nil || b.Online {
		t.Fatalf("B should be offline but kept: %+v", b)
	}
}

// TestSetCustomName pins the optional rename: a custom name overrides the
// auto-name, survives a re-sync, and clearing reverts to the auto-name.
func TestSetCustomName(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	if err := s.Sync(ctx, []Detected{readerA()}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := s.SetCustomName(ctx, "nfc:i2c-1", "  Haustür  "); err != nil {
		t.Fatalf("SetCustomName: %v", err)
	}
	r, _ := s.Get(ctx, "nfc:i2c-1")
	if r.CustomName != "Haustür" || r.DisplayName() != "Haustür" {
		t.Fatalf("custom name not applied: %+v", r)
	}
	// A re-sync (restart) must not clobber the custom name, and the
	// auto-name is still updated underneath.
	if err := s.Sync(ctx, []Detected{readerA()}); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	r, _ = s.Get(ctx, "nfc:i2c-1")
	if r.CustomName != "Haustür" || r.Name != "RPi-NFC-PN532 (I2C-1)" {
		t.Fatalf("re-sync clobbered names: %+v", r)
	}
	// Clearing reverts to the auto-name.
	if err := s.SetCustomName(ctx, "nfc:i2c-1", ""); err != nil {
		t.Fatalf("clear custom name: %v", err)
	}
	r, _ = s.Get(ctx, "nfc:i2c-1")
	if r.CustomName != "" || r.DisplayName() != r.Name {
		t.Fatalf("clear did not revert: %+v", r)
	}
	// Unknown reader errors.
	if err := s.SetCustomName(ctx, "nfc:i2c-9", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCustomName unknown = %v, want ErrNotFound", err)
	}
}

// TestNoteTag pins the last-seen tracking and its no-op on an unknown
// reader.
func TestNoteTag(t *testing.T) {
	ctx := context.Background()
	s, c := newTestStore(t)
	if err := s.Sync(ctx, []Detected{readerA()}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	c.advance(time.Minute)
	if err := s.NoteTag(ctx, "nfc:i2c-1", "04:A3:1B:2C"); err != nil {
		t.Fatalf("NoteTag: %v", err)
	}
	r, err := s.Get(ctx, "nfc:i2c-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.LastUID != "04:A3:1B:2C" || r.LastSeenAt == 0 {
		t.Fatalf("last tag not recorded: %+v", r)
	}
	if err := s.NoteTag(ctx, "nfc:i2c-9", "AA:BB"); err != nil {
		t.Fatalf("NoteTag unknown reader errored: %v", err)
	}
	if _, err := s.Get(ctx, "nfc:i2c-9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown reader was created: %v", err)
	}
}

// TestSync_RaceSmoke runs concurrent NoteTag against a live registry
// alongside a re-sync, the -race guard for the continuous observer path
// racing the startup reconcile.
func TestSync_RaceSmoke(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	if err := s.Sync(ctx, []Detected{readerA()}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.NoteTag(ctx, "nfc:i2c-1", "D6:45:90:3B")
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.Sync(ctx, []Detected{readerA()})
	}()
	wg.Wait()
	if _, err := s.List(ctx); err != nil {
		t.Fatalf("List after race: %v", err)
	}
}
