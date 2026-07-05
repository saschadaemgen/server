package readerstore

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/designerstore"
)

// clock is a controllable test clock so timestamps are deterministic
// and first_seen/last_seen invariants can be asserted precisely.
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
// (including the 036 readers table and the 032 System/Reader seed)
// runs, and returns the reader registry, a designer store to provide a
// real ensureGraph (satisfying the graph_id foreign key), and the clock.
func newTestStore(t *testing.T) (*Store, *designerstore.Store, *clock) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	c := &clock{t: time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)}
	ds := designerstore.New(d.DB, designerstore.WithClock(c.now))
	return New(d.DB, WithClock(c.now)), ds, c
}

// countingEnsure wraps a real ensureGraph and counts invocations, so a
// test can assert restarts do NOT re-create the reader's graph.
func countingEnsure(ds *designerstore.Store, calls *int) func(context.Context, string) (int64, error) {
	return func(ctx context.Context, name string) (int64, error) {
		*calls++
		return ds.EnsureReaderGraph(ctx, name)
	}
}

func readerA() Detected {
	return Detected{ID: "nfc:i2c-1", Kind: "nfc", Model: "PN532", Firmware: "1.6", Bus: "i2c-1", Name: "PN532 · i2c-1"}
}
func readerB() Detected {
	return Detected{ID: "nfc:i2c-2", Kind: "nfc", Model: "PN532", Firmware: "1.6", Bus: "i2c-2", Name: "PN532 · i2c-2"}
}

func byID(readers []Reader, id string) *Reader {
	for i := range readers {
		if readers[i].ID == id {
			return &readers[i]
		}
	}
	return nil
}

// readerGraphCount returns how many graphs live in the System/Reader
// folder - the duplicate-detection assertion.
func readerGraphCount(t *testing.T, ds *designerstore.Store) int {
	t.Helper()
	folders, graphs, err := ds.Tree(context.Background())
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	var readerFolder int64
	for _, f := range folders {
		if f.Name == "Reader" && f.System {
			readerFolder = f.ID
		}
	}
	if readerFolder == 0 {
		t.Fatal("System/Reader folder not found")
	}
	n := 0
	for _, g := range graphs {
		if g.FolderID == readerFolder {
			n++
		}
	}
	return n
}

// TestSync_AutoRegistersOnlineWithGraph pins Teil A's auto-registration:
// a simulated detection creates an online row plus a structure-locked
// System/Reader graph the NFC page can jump to.
func TestSync_AutoRegistersOnlineWithGraph(t *testing.T) {
	ctx := context.Background()
	s, ds, _ := newTestStore(t)
	calls := 0
	if err := s.Sync(ctx, []Detected{readerA()}, countingEnsure(ds, &calls)); err != nil {
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
	if r.ID != "nfc:i2c-1" || !r.Online || r.Model != "PN532" || r.Bus != "i2c-1" {
		t.Fatalf("row = %+v, want online PN532 on i2c-1", r)
	}
	if r.GraphID == 0 {
		t.Fatal("reader has no linked System/Reader graph")
	}
	if r.FirstSeenAt == 0 {
		t.Fatal("first_seen_at not set")
	}
	if calls != 1 {
		t.Fatalf("ensureGraph called %d times, want 1", calls)
	}
	if n := readerGraphCount(t, ds); n != 1 {
		t.Fatalf("System/Reader graphs = %d, want 1", n)
	}
}

// TestSync_StableIdentityNoDuplicate pins the mandatory "second start =
// no duplicate": the same hardware re-detected reuses the row, the
// graph id, and never spawns a second graph, and ensureGraph is not
// called again (the persisted link short-circuits it).
func TestSync_StableIdentityNoDuplicate(t *testing.T) {
	ctx := context.Background()
	s, ds, c := newTestStore(t)
	calls := 0
	ensure := countingEnsure(ds, &calls)

	if err := s.Sync(ctx, []Detected{readerA()}, ensure); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	first, _ := s.List(ctx)
	firstGraph := first[0].GraphID
	firstSeen := first[0].FirstSeenAt

	c.advance(time.Hour) // a later restart
	if err := s.Sync(ctx, []Detected{readerA()}, ensure); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	second, _ := s.List(ctx)
	if len(second) != 1 {
		t.Fatalf("after restart readers = %d, want 1 (no duplicate)", len(second))
	}
	if second[0].GraphID != firstGraph {
		t.Fatalf("graph id changed on restart: %d -> %d", firstGraph, second[0].GraphID)
	}
	if second[0].FirstSeenAt != firstSeen {
		t.Fatalf("first_seen_at moved on restart: %d -> %d", firstSeen, second[0].FirstSeenAt)
	}
	if calls != 1 {
		t.Fatalf("ensureGraph called %d times across two syncs, want 1", calls)
	}
	if n := readerGraphCount(t, ds); n != 1 {
		t.Fatalf("System/Reader graphs after restart = %d, want 1", n)
	}
}

// TestSync_HardwareGoneStaysOffline pins Teil A's offline rule: a reader
// no longer detected keeps its row (not deleted), flips to offline, and
// preserves its history; reappearing flips it back online with the same
// identity and graph.
func TestSync_HardwareGoneStaysOffline(t *testing.T) {
	ctx := context.Background()
	s, ds, c := newTestStore(t)
	ensure := countingEnsure(ds, new(int))

	if err := s.Sync(ctx, []Detected{readerA()}, ensure); err != nil {
		t.Fatalf("Sync online: %v", err)
	}
	if err := s.NoteTag(ctx, "nfc:i2c-1", "D6:45:90:3B"); err != nil {
		t.Fatalf("NoteTag: %v", err)
	}
	before, _ := s.List(ctx)
	graphID := before[0].GraphID

	// Hardware gone: empty detection set.
	c.advance(time.Minute)
	if err := s.Sync(ctx, nil, ensure); err != nil {
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

	// Reappears: back online, same identity + graph.
	c.advance(time.Minute)
	if err := s.Sync(ctx, []Detected{readerA()}, ensure); err != nil {
		t.Fatalf("Sync back online: %v", err)
	}
	back, _ := s.List(ctx)
	if len(back) != 1 || !back[0].Online {
		t.Fatalf("reader did not come back online: %+v", back)
	}
	if back[0].GraphID != graphID {
		t.Fatalf("graph id changed after reappear: %d -> %d", graphID, back[0].GraphID)
	}
}

// TestSync_PartialSetOnlyDetectedOnline pins that a mixed sync flips
// exactly the absent readers offline and leaves the present one online.
func TestSync_PartialSetOnlyDetectedOnline(t *testing.T) {
	ctx := context.Background()
	s, ds, _ := newTestStore(t)
	ensure := countingEnsure(ds, new(int))

	if err := s.Sync(ctx, []Detected{readerA(), readerB()}, ensure); err != nil {
		t.Fatalf("Sync both: %v", err)
	}
	if err := s.Sync(ctx, []Detected{readerA()}, ensure); err != nil {
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

// TestNoteTag pins the last-seen tracking and its no-op on an unknown
// reader.
func TestNoteTag(t *testing.T) {
	ctx := context.Background()
	s, ds, c := newTestStore(t)
	if err := s.Sync(ctx, []Detected{readerA()}, countingEnsure(ds, new(int))); err != nil {
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
	// An unregistered reader is a silent no-op, not an error, and does
	// not conjure a row.
	if err := s.NoteTag(ctx, "nfc:i2c-9", "AA:BB"); err != nil {
		t.Fatalf("NoteTag unknown reader errored: %v", err)
	}
	if _, err := s.Get(ctx, "nfc:i2c-9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown reader was created: %v", err)
	}
}

// TestSync_RaceSmoke runs concurrent NoteTag against a live registry
// alongside a re-sync, the -race guard for the observer path racing the
// startup reconcile.
func TestSync_RaceSmoke(t *testing.T) {
	ctx := context.Background()
	s, ds, _ := newTestStore(t)
	ensure := countingEnsure(ds, new(int))
	if err := s.Sync(ctx, []Detected{readerA()}, ensure); err != nil {
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
		_ = s.Sync(ctx, []Detected{readerA()}, ensure)
	}()
	wg.Wait()
	if _, err := s.List(ctx); err != nil {
		t.Fatalf("List after race: %v", err)
	}
}
