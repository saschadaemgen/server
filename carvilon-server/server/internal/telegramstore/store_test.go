package telegramstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
)

func newTestStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d.DB, opts...)
}

// testClock returns a deterministic clock and a function to advance it.
func testClock(startMilli int64) (func() time.Time, func(ms int64)) {
	cur := time.UnixMilli(startMilli)
	return func() time.Time { return cur },
		func(ms int64) { cur = cur.Add(time.Duration(ms) * time.Millisecond) }
}

func TestAddRemoveAllowed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.AddAllowed(ctx, 42, "Wohnzimmer"); err != nil {
		t.Fatalf("AddAllowed: %v", err)
	}
	// Duplicate rejected.
	if err := s.AddAllowed(ctx, 42, "anders"); err != ErrChatExists {
		t.Fatalf("duplicate AddAllowed = %v, want ErrChatExists", err)
	}

	if err := s.RemoveAllowed(ctx, 42); err != nil {
		t.Fatalf("RemoveAllowed: %v", err)
	}
	// Missing chat -> not found.
	if err := s.RemoveAllowed(ctx, 42); err != ErrChatNotFound {
		t.Fatalf("RemoveAllowed missing = %v, want ErrChatNotFound", err)
	}
}

func TestListAllowedOrdering(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Insert out of order: order must be label, then chat ID.
	if err := s.AddAllowed(ctx, 1, "beta"); err != nil {
		t.Fatalf("AddAllowed: %v", err)
	}
	if err := s.AddAllowed(ctx, 3, "alpha"); err != nil {
		t.Fatalf("AddAllowed: %v", err)
	}
	if err := s.AddAllowed(ctx, 2, "alpha"); err != nil {
		t.Fatalf("AddAllowed: %v", err)
	}

	list, err := s.ListAllowed(ctx)
	if err != nil {
		t.Fatalf("ListAllowed: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListAllowed len = %d, want 3", len(list))
	}
	wantIDs := []int64{2, 3, 1}
	wantLabels := []string{"alpha", "alpha", "beta"}
	for i, c := range list {
		if c.ChatID != wantIDs[i] || c.Label != wantLabels[i] {
			t.Errorf("ListAllowed[%d] = (%d, %q), want (%d, %q)",
				i, c.ChatID, c.Label, wantIDs[i], wantLabels[i])
		}
	}
}

func TestLoadAllowlistSnapshot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.AddAllowed(ctx, 7, "Sascha"); err != nil {
		t.Fatalf("AddAllowed: %v", err)
	}
	if err := s.AddAllowed(ctx, 9, "  Haus  "); err != nil {
		t.Fatalf("AddAllowed: %v", err)
	}

	snap, err := s.LoadAllowlist(ctx)
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}
	if len(snap) != 2 {
		t.Fatalf("LoadAllowlist len = %d, want 2", len(snap))
	}
	if snap[7] != "Sascha" {
		t.Errorf("snap[7] = %q, want %q", snap[7], "Sascha")
	}
	// Labels are trimmed on write.
	if snap[9] != "Haus" {
		t.Errorf("snap[9] = %q, want %q", snap[9], "Haus")
	}
	if _, ok := snap[8]; ok {
		t.Error("snap must not contain chat 8")
	}
}

func TestUpsertPendingInsertAndRefresh(t *testing.T) {
	ctx := context.Background()
	now, advance := testClock(1_000)
	s := newTestStore(t, WithClock(now))

	if err := s.UpsertPending(ctx, 42, "alice", "Alice"); err != nil {
		t.Fatalf("UpsertPending insert: %v", err)
	}
	advance(5_000)
	if err := s.UpsertPending(ctx, 42, "alice_new", "Alicia"); err != nil {
		t.Fatalf("UpsertPending refresh: %v", err)
	}

	list, err := s.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListPending len = %d, want 1", len(list))
	}
	c := list[0]
	if c.ChatID != 42 {
		t.Errorf("ChatID = %d, want 42", c.ChatID)
	}
	if c.Username != "alice_new" || c.FirstName != "Alicia" {
		t.Errorf("metadata = (%q, %q), want refreshed (alice_new, Alicia)", c.Username, c.FirstName)
	}
	if c.FirstSeen != 1_000 {
		t.Errorf("FirstSeen = %d, want 1000 (must stay at first contact)", c.FirstSeen)
	}
	if c.LastSeen != 6_000 {
		t.Errorf("LastSeen = %d, want 6000 (must advance)", c.LastSeen)
	}
	if c.Rejected {
		t.Error("fresh pending chat must not be rejected")
	}
}

func TestUpsertPendingEmptyMetadata(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Empty username/first_name are stored as NULL and read back as "".
	if err := s.UpsertPending(ctx, 5, "", ""); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	list, err := s.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListPending len = %d, want 1", len(list))
	}
	if list[0].Username != "" || list[0].FirstName != "" {
		t.Errorf("metadata = (%q, %q), want empty", list[0].Username, list[0].FirstName)
	}
}

func TestRejectedSurvivesRefresh(t *testing.T) {
	ctx := context.Background()
	now, advance := testClock(1_000)
	s := newTestStore(t, WithClock(now))

	if err := s.UpsertPending(ctx, 13, "spam", "Spammer"); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	if err := s.Reject(ctx, 13); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	// The chat keeps writing: it must stay rejected, not resurface.
	advance(60_000)
	if err := s.UpsertPending(ctx, 13, "spam2", "Spammer"); err != nil {
		t.Fatalf("UpsertPending refresh: %v", err)
	}

	list, err := s.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListPending len = %d, want 1", len(list))
	}
	c := list[0]
	if !c.Rejected {
		t.Error("rejected flag must survive an UpsertPending refresh")
	}
	if c.LastSeen != 61_000 {
		t.Errorf("LastSeen = %d, want 61000 (refresh still advances it)", c.LastSeen)
	}
	if c.Username != "spam2" {
		t.Errorf("Username = %q, want spam2 (refresh still updates metadata)", c.Username)
	}
}

func TestApproveMovesPendingToAllowed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpsertPending(ctx, 42, "alice", "Alice"); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	if err := s.Approve(ctx, 42, "Alice privat"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Allowed now.
	snap, err := s.LoadAllowlist(ctx)
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}
	if snap[42] != "Alice privat" {
		t.Errorf("snap[42] = %q, want %q", snap[42], "Alice privat")
	}
	// Pending row gone.
	pending, err := s.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after approve = %d rows, want 0", len(pending))
	}

	// Approving a chat that is not pending -> not found.
	if err := s.Approve(ctx, 99, "ghost"); err != ErrChatNotFound {
		t.Fatalf("Approve missing = %v, want ErrChatNotFound", err)
	}
}

// TestApproveAlreadyAllowedClearsPending: a chat added manually while
// also sitting on the pending list counts as approved - the pending
// row is cleared (otherwise the card would be stuck as "wartend"
// forever) and the existing allowlist entry, label included, wins.
func TestApproveAlreadyAllowedClearsPending(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.AddAllowed(ctx, 42, "schon da"); err != nil {
		t.Fatalf("AddAllowed: %v", err)
	}
	if err := s.UpsertPending(ctx, 42, "alice", "Alice"); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}

	if err := s.Approve(ctx, 42, "doppelt"); err != nil {
		t.Fatalf("Approve already-allowed = %v, want success (pending cleanup)", err)
	}

	// The pending row is gone...
	pending, err := s.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after approve = %+v, want empty", pending)
	}
	// ...and the original allowlist entry is untouched.
	snap, err := s.LoadAllowlist(ctx)
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}
	if snap[42] != "schon da" {
		t.Errorf("snap[42] = %q, want original label %q", snap[42], "schon da")
	}
}

func TestReject(t *testing.T) {
	ctx := context.Background()
	now, _ := testClock(5_000)
	s := newTestStore(t, WithClock(now))

	if err := s.UpsertPending(ctx, 13, "spam", ""); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	if err := s.Reject(ctx, 13); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	list, err := s.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(list) != 1 || !list[0].Rejected {
		t.Fatalf("ListPending = %+v, want one rejected row", list)
	}
	// Missing chat -> not found.
	if err := s.Reject(ctx, 99); err != ErrChatNotFound {
		t.Fatalf("Reject missing = %v, want ErrChatNotFound", err)
	}
}

func TestPendingCapEviction(t *testing.T) {
	ctx := context.Background()
	now, advance := testClock(1_000)
	s := newTestStore(t, WithClock(now))

	// Three rejected chats first - they must not count against the cap
	// and must not be evicted.
	for _, id := range []int64{9001, 9002, 9003} {
		if err := s.UpsertPending(ctx, id, "rejected", ""); err != nil {
			t.Fatalf("UpsertPending rejected %d: %v", id, err)
		}
		if err := s.Reject(ctx, id); err != nil {
			t.Fatalf("Reject %d: %v", id, err)
		}
		advance(1)
	}

	// 105 waiting chats, each with a strictly later last_seen.
	for id := int64(1); id <= 105; id++ {
		advance(1)
		if err := s.UpsertPending(ctx, id, "", ""); err != nil {
			t.Fatalf("UpsertPending %d: %v", id, err)
		}
	}

	list, err := s.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	waiting := map[int64]bool{}
	rejected := map[int64]bool{}
	for _, c := range list {
		if c.Rejected {
			rejected[c.ChatID] = true
		} else {
			waiting[c.ChatID] = true
		}
	}
	if len(waiting) != pendingCap {
		t.Fatalf("waiting rows = %d, want %d", len(waiting), pendingCap)
	}
	// Newest 100 remain: 6..105. Oldest five (1..5) evicted.
	for id := int64(1); id <= 5; id++ {
		if waiting[id] {
			t.Errorf("chat %d should have been evicted", id)
		}
	}
	for id := int64(6); id <= 105; id++ {
		if !waiting[id] {
			t.Errorf("chat %d should have survived the cap", id)
		}
	}
	// Rejected rows are untouched by the cap.
	if len(rejected) != 3 || !rejected[9001] || !rejected[9002] || !rejected[9003] {
		t.Errorf("rejected rows = %v, want 9001-9003 untouched", rejected)
	}
}

func TestNegativeGroupChatIDs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Telegram group chat IDs are negative 64-bit values.
	const group = int64(-1001234567890)

	// Pending -> approve round-trip.
	if err := s.UpsertPending(ctx, group, "", "Hausgruppe"); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	pending, err := s.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 1 || pending[0].ChatID != group {
		t.Fatalf("ListPending = %+v, want chat %d", pending, group)
	}
	if err := s.Approve(ctx, group, "Hausgruppe"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Allowlist round-trip.
	snap, err := s.LoadAllowlist(ctx)
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}
	if snap[group] != "Hausgruppe" {
		t.Errorf("snap[%d] = %q, want %q", group, snap[group], "Hausgruppe")
	}
	list, err := s.ListAllowed(ctx)
	if err != nil {
		t.Fatalf("ListAllowed: %v", err)
	}
	if len(list) != 1 || list[0].ChatID != group {
		t.Fatalf("ListAllowed = %+v, want chat %d", list, group)
	}
	if err := s.RemoveAllowed(ctx, group); err != nil {
		t.Fatalf("RemoveAllowed: %v", err)
	}
}
