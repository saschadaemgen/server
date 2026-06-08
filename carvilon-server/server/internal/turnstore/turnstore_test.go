package turnstore

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestStore opens a real migrated carvilon DB in a temp file so the
// turn_events / ice_state_events tables come from Migration 019 (no
// hand-rolled DDL, no schema drift).
func newTestStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "turn.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return NewStore(database.DB), database
}

func TestStore_InsertAndRecentEvents(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Truncate(time.Millisecond)
	yes := true

	// Oldest -> newest, so RecentEvents must return them reversed.
	events := []Event{
		{Kind: "allocation_created", Time: base.Add(-2 * time.Minute), SrcMasked: "v4:203.0.x.x#a1", DstMasked: "v4:198.51.x.x#b2", Protocol: "udp", Username: "carvilon", Realm: "carvilon"},
		{Kind: "auth", Time: base.Add(-1 * time.Minute), SrcMasked: "v4:203.0.x.x#a1", Protocol: "udp", Username: "carvilon", Realm: "carvilon", AuthOK: &yes},
		{Kind: "allocation_error", Time: base, SrcMasked: "v4:203.0.x.x#a1", Protocol: "tcp", Err: "read timeout"},
	}
	for _, e := range events {
		if err := store.InsertEvent(ctx, e); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}

	got, err := store.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}
	if got[0].Kind != "allocation_error" || got[2].Kind != "allocation_created" {
		t.Fatalf("wrong order: %q .. %q", got[0].Kind, got[2].Kind)
	}
	// auth row carries AuthOK; the others must read it back as nil.
	if got[1].AuthOK == nil || *got[1].AuthOK != true {
		t.Fatalf("auth event AuthOK = %v, want true", got[1].AuthOK)
	}
	if got[0].AuthOK != nil {
		t.Fatalf("non-auth event AuthOK = %v, want nil", got[0].AuthOK)
	}
	// masked address round-trips; an empty optional (DstMasked on the
	// error row) reads back as "".
	if got[2].SrcMasked != "v4:203.0.x.x#a1" || got[2].DstMasked != "v4:198.51.x.x#b2" {
		t.Fatalf("masked round-trip failed: src=%q dst=%q", got[2].SrcMasked, got[2].DstMasked)
	}
	if got[0].DstMasked != "" {
		t.Fatalf("error event DstMasked = %q, want empty", got[0].DstMasked)
	}
	if got[0].Err != "read timeout" {
		t.Fatalf("error event Err = %q", got[0].Err)
	}
}

func TestStore_RecentICEEvents(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Truncate(time.Millisecond)
	for i, st := range []string{"checking", "connected", "failed"} {
		if err := store.InsertICEEvent(ctx, ICEEvent{
			StreamID: "0c:ea:14:42:42:99", State: st,
			Time: base.Add(time.Duration(i) * time.Second), SinceStartMS: int64(i * 100),
		}); err != nil {
			t.Fatalf("InsertICEEvent: %v", err)
		}
	}
	got, err := store.RecentICEEvents(ctx, 10)
	if err != nil {
		t.Fatalf("RecentICEEvents: %v", err)
	}
	if len(got) != 3 || got[0].State != "failed" {
		t.Fatalf("want newest-first 3 ice events, got %d (first %q)", len(got), firstState(got))
	}
	if got[0].SinceStartMS != 200 {
		t.Fatalf("SinceStartMS = %d, want 200", got[0].SinceStartMS)
	}
}

func firstState(s []ICEEvent) string {
	if len(s) == 0 {
		return ""
	}
	return s[0].State
}

// TestStore_SchemaHasNoRawIPColumn is the structural privacy guard:
// the turn_events table must hold only masked address columns, never a
// raw-IP one. If a future migration ever adds src_addr/dst_addr/raw,
// this fails loudly.
func TestStore_SchemaHasNoRawIPColumn(t *testing.T) {
	_, database := newTestStore(t)
	rows, err := database.DB.Query("PRAGMA table_info(turn_events)")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[name] = true
	}
	for _, banned := range []string{"src_addr", "dst_addr", "src_raw", "dst_raw", "raw_ip", "ip"} {
		if cols[banned] {
			t.Fatalf("turn_events must not have a raw-IP column, found %q", banned)
		}
	}
	if !cols["src_masked"] || !cols["dst_masked"] {
		t.Fatalf("turn_events missing masked columns: %v", cols)
	}
}

func TestStore_PurgeRetention(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	// One row well outside the window, one inside.
	old := Event{Kind: "auth", Time: now.Add(-40 * 24 * time.Hour), SrcMasked: "v4:old"}
	fresh := Event{Kind: "auth", Time: now, SrcMasked: "v4:fresh"}
	if err := store.InsertEvent(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertEvent(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertICEEvent(ctx, ICEEvent{StreamID: "x", State: "connected", Time: now.Add(-40 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	n, err := store.Purge(ctx, now.Add(-DefaultRetention))
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged %d rows, want 2 (one event + one ice)", n)
	}
	got, _ := store.RecentEvents(ctx, 10)
	if len(got) != 1 || got[0].SrcMasked != "v4:fresh" {
		t.Fatalf("after purge want only the fresh event, got %+v", got)
	}
}

// TestWriter_ConcurrentSubmit_NoRace drives many goroutines into the
// non-blocking Submit* paths while the single Run goroutine drains to
// SQLite. Run under `go test -race`. The buffer is sized above the
// total so nothing is dropped and the final counts are exact.
func TestWriter_ConcurrentSubmit_NoRace(t *testing.T) {
	store, _ := newTestStore(t)
	w := NewWriter(store, testLogger(), Options{BufferSize: 8192, Retention: time.Hour, PurgeInterval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// The -race goal is concurrency safety, not throughput: a moderate
	// burst across many goroutines exercises the channels and the store
	// without paying for thousands of per-insert SQLite commits.
	const goroutines, perG = 8, 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				w.SubmitEvent(Event{Kind: "auth", Time: time.Now(), SrcMasked: "v4:x", Username: "carvilon"})
				w.SubmitICE(ICEEvent{StreamID: "aa", State: "connected", Time: time.Now()})
			}
		}()
	}
	wg.Wait()

	// Poll a cheap COUNT(*) (white-box: store.db) instead of scanning
	// the rows back, so the reader does not starve the single-connection
	// writer while it drains.
	want := goroutines * perG
	deadline := time.Now().Add(20 * time.Second)
	for {
		if countRows(t, store, "turn_events") >= want && countRows(t, store, "ice_state_events") >= want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain timeout: events=%d ice=%d want=%d",
				countRows(t, store, "turn_events"), countRows(t, store, "ice_state_events"), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

func countRows(t *testing.T, store *Store, table string) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestSnapshotHolder_Concurrent(t *testing.T) {
	h := NewSnapshotHolder()
	if _, _, present := h.Get(); present {
		t.Fatal("fresh holder must report no snapshot")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(n int) { defer wg.Done(); h.Set(Snapshot{Enabled: true, AllocationCount: n}, time.Now()) }(i)
		go func() { defer wg.Done(); _, _, _ = h.Get() }()
	}
	wg.Wait()
	if _, _, present := h.Get(); !present {
		t.Fatal("after Set the holder must report present")
	}
}

func TestFreshness(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name       string
		received   time.Time
		wantAge    int
		wantStale  bool
		staleAfter time.Duration
	}{
		{"fresh", now.Add(-5 * time.Second), 5, false, DefaultStaleAfter},
		{"stale", now.Add(-45 * time.Second), 45, true, DefaultStaleAfter},
		{"edge of window", now.Add(-30 * time.Second), 30, false, DefaultStaleAfter},
		{"future skew clamps to zero", now.Add(2 * time.Second), 0, false, DefaultStaleAfter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			age, stale := Freshness(c.received, now, c.staleAfter)
			if age != c.wantAge || stale != c.wantStale {
				t.Fatalf("Freshness = (%d, %v), want (%d, %v)", age, stale, c.wantAge, c.wantStale)
			}
		})
	}
}
