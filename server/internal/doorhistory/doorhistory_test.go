package doorhistory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
)

const testMockMAC = "0c:ea:14:42:42:42"
const testMockMACB = "0c:ea:14:42:42:43"

func newStore(t *testing.T) (*SQLStore, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	now := int64(1747000000000)
	if _, err := d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at) VALUES (?, ?, ?, 'web', ?, ?)`,
		testMockMAC, "Familie Mueller 2OG", 8100, now, now,
	); err != nil {
		t.Fatalf("seed viewer A: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at) VALUES (?, ?, ?, 'web', ?, ?)`,
		testMockMACB, "Wohnung 2", 8101, now, now,
	); err != nil {
		t.Fatalf("seed viewer B: %v", err)
	}
	return NewSQLStore(d.DB), d
}

func TestInsert_RoundTrip(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, err := s.Insert(ctx, Event{
		MockMAC:     testMockMAC,
		EventType:   TypeDoorbellStart,
		IntercomMAC: "28:70:4e:31:e2:9c",
		OccurredAt:  time.Unix(1747000000, 0),
		CancelToken: "tok-1",
		RoomID:      "WR-0cea14000001-abcdef",
	}, []byte("raw"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id == 0 {
		t.Error("Insert returned id=0")
	}
	events, err := s.ListForMock(ctx, testMockMAC, 10)
	if err != nil {
		t.Fatalf("ListForMock: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("ListForMock returned %d events, want 1", len(events))
	}
	got := events[0]
	if got.EventType != TypeDoorbellStart {
		t.Errorf("EventType = %q", got.EventType)
	}
	if got.IntercomMAC != "28:70:4e:31:e2:9c" {
		t.Errorf("IntercomMAC = %q", got.IntercomMAC)
	}
	if got.CancelToken != "tok-1" {
		t.Errorf("CancelToken = %q", got.CancelToken)
	}
	if got.RoomID != "WR-0cea14000001-abcdef" {
		t.Errorf("RoomID = %q", got.RoomID)
	}
	if got.CancelledAt != nil {
		t.Errorf("CancelledAt = %v, want nil", got.CancelledAt)
	}
	if got.ReadAt != nil {
		t.Errorf("ReadAt = %v, want nil", got.ReadAt)
	}
}

func TestInsert_RejectsEmptyFields(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.Insert(ctx, Event{EventType: TypeDoorbellStart}, nil); err == nil {
		t.Error("Insert with empty mock_mac succeeded")
	}
	if _, err := s.Insert(ctx, Event{MockMAC: testMockMAC}, nil); err == nil {
		t.Error("Insert with empty event_type succeeded")
	}
}

func TestUpdateCancel_MatchesNewestOpen(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	older, err := s.Insert(ctx, Event{
		MockMAC:     testMockMAC,
		EventType:   TypeDoorbellStart,
		OccurredAt:  time.Unix(1747000000, 0),
		CancelToken: "tok-shared",
	}, nil)
	if err != nil {
		t.Fatalf("insert older: %v", err)
	}
	newer, err := s.Insert(ctx, Event{
		MockMAC:     testMockMAC,
		EventType:   TypeDoorbellStart,
		OccurredAt:  time.Unix(1747001000, 0),
		CancelToken: "tok-shared",
	}, nil)
	if err != nil {
		t.Fatalf("insert newer: %v", err)
	}
	if err := s.UpdateCancel(ctx, testMockMAC, "tok-shared", time.Unix(1747001100, 0)); err != nil {
		t.Fatalf("UpdateCancel: %v", err)
	}
	events, err := s.ListForMock(ctx, testMockMAC, 10)
	if err != nil {
		t.Fatalf("ListForMock: %v", err)
	}
	byID := map[int64]Event{}
	for _, ev := range events {
		byID[ev.ID] = ev
	}
	if byID[newer].CancelledAt == nil {
		t.Error("newer event still has CancelledAt nil")
	}
	if byID[older].CancelledAt != nil {
		t.Error("older event was cancelled but newer one should have matched first")
	}
}

func TestUpdateCancel_UnknownTokenReturnsNotFound(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.Insert(ctx, Event{
		MockMAC:     testMockMAC,
		EventType:   TypeDoorbellStart,
		OccurredAt:  time.Unix(1747000000, 0),
		CancelToken: "tok-1",
	}, nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	err := s.UpdateCancel(ctx, testMockMAC, "tok-nope", time.Unix(1747001000, 0))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateCancel for unknown token returned %v, want ErrNotFound", err)
	}
}

func TestUnreadCount_OnlyUnread(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := s.Insert(ctx, Event{
			MockMAC:    testMockMAC,
			EventType:  TypeDoorbellStart,
			OccurredAt: time.Unix(1747000000+int64(i), 0),
		}, nil)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	n, err := s.UnreadCount(ctx, testMockMAC)
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 3 {
		t.Errorf("UnreadCount = %d, want 3", n)
	}
	if err := s.MarkRead(ctx, testMockMAC, ids[:2]); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	n, err = s.UnreadCount(ctx, testMockMAC)
	if err != nil {
		t.Fatalf("UnreadCount after mark: %v", err)
	}
	if n != 1 {
		t.Errorf("UnreadCount after mark = %d, want 1", n)
	}
}

func TestMarkRead_RespectsMockScope(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	idA, err := s.Insert(ctx, Event{
		MockMAC:    testMockMAC,
		EventType:  TypeDoorbellStart,
		OccurredAt: time.Unix(1747000000, 0),
	}, nil)
	if err != nil {
		t.Fatalf("insert A: %v", err)
	}
	// Try to mark mock A's event as read while claiming to be mock B.
	if err := s.MarkRead(ctx, testMockMACB, []int64{idA}); err != nil {
		t.Fatalf("MarkRead cross-mock: %v", err)
	}
	n, err := s.UnreadCount(ctx, testMockMAC)
	if err != nil {
		t.Fatalf("UnreadCount A: %v", err)
	}
	if n != 1 {
		t.Errorf("cross-mock MarkRead leaked: UnreadCount(A) = %d, want 1", n)
	}
}

func TestMarkAllRead_Resets(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if _, err := s.Insert(ctx, Event{
			MockMAC:    testMockMAC,
			EventType:  TypeDoorbellStart,
			OccurredAt: time.Unix(1747000000+int64(i), 0),
		}, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := s.MarkAllRead(ctx, testMockMAC, time.Unix(1747100000, 0)); err != nil {
		t.Fatalf("MarkAllRead: %v", err)
	}
	n, err := s.UnreadCount(ctx, testMockMAC)
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 0 {
		t.Errorf("UnreadCount after MarkAllRead = %d, want 0", n)
	}
}

func TestListForMock_NewestFirstAndLimit(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.Insert(ctx, Event{
			MockMAC:    testMockMAC,
			EventType:  TypeDoorbellStart,
			OccurredAt: time.Unix(1747000000+int64(i), 0),
		}, nil); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	events, err := s.ListForMock(ctx, testMockMAC, 3)
	if err != nil {
		t.Fatalf("ListForMock: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("len = %d, want 3", len(events))
	}
	// Newest first.
	if events[0].OccurredAt.Unix() != 1747000004 {
		t.Errorf("first = %d, want 1747000004", events[0].OccurredAt.Unix())
	}
	if events[2].OccurredAt.Unix() != 1747000002 {
		t.Errorf("third = %d, want 1747000002", events[2].OccurredAt.Unix())
	}
}

func TestListForMock_FiltersByMock(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.Insert(ctx, Event{
		MockMAC:    testMockMAC,
		EventType:  TypeDoorbellStart,
		OccurredAt: time.Unix(1747000000, 0),
	}, nil); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	if _, err := s.Insert(ctx, Event{
		MockMAC:    testMockMACB,
		EventType:  TypeDoorbellStart,
		OccurredAt: time.Unix(1747000001, 0),
	}, nil); err != nil {
		t.Fatalf("insert B: %v", err)
	}
	got, err := s.ListForMock(ctx, testMockMAC, 10)
	if err != nil {
		t.Fatalf("ListForMock: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(A) = %d, want 1", len(got))
	}
	if got[0].MockMAC != testMockMAC {
		t.Errorf("leaked mock B event into mock A list")
	}
}

func TestAggregateAdmin_BucketsByWindow(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	now := time.Unix(1747100000, 0)
	insertAt := func(macAddr string, offset time.Duration) {
		if _, err := s.Insert(ctx, Event{
			MockMAC:    macAddr,
			EventType:  TypeDoorbellStart,
			OccurredAt: now.Add(offset),
		}, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insertAt(testMockMAC, -1*time.Hour)
	insertAt(testMockMAC, -2*time.Hour)
	insertAt(testMockMACB, -30*time.Minute)
	insertAt(testMockMAC, -3*24*time.Hour)
	insertAt(testMockMAC, -10*24*time.Hour)
	insertAt(testMockMAC, -40*24*time.Hour)
	stats, err := s.AggregateAdmin(ctx, now)
	if err != nil {
		t.Fatalf("AggregateAdmin: %v", err)
	}
	if stats.Total24h != 3 {
		t.Errorf("Total24h = %d, want 3", stats.Total24h)
	}
	if stats.Total7d != 4 {
		t.Errorf("Total7d = %d, want 4", stats.Total7d)
	}
	if stats.Total30d != 5 {
		t.Errorf("Total30d = %d, want 5", stats.Total30d)
	}
	if stats.PerMock24h[testMockMAC] != 2 {
		t.Errorf("PerMock24h[A] = %d, want 2", stats.PerMock24h[testMockMAC])
	}
	if stats.PerMock24h[testMockMACB] != 1 {
		t.Errorf("PerMock24h[B] = %d, want 1", stats.PerMock24h[testMockMACB])
	}
}

func TestAggregateAdmin_IgnoresCancelEvents(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	now := time.Unix(1747100000, 0)
	if _, err := s.Insert(ctx, Event{
		MockMAC:    testMockMAC,
		EventType:  TypeDoorbellCancel,
		OccurredAt: now.Add(-1 * time.Hour),
	}, nil); err != nil {
		t.Fatalf("insert cancel: %v", err)
	}
	stats, err := s.AggregateAdmin(ctx, now)
	if err != nil {
		t.Fatalf("AggregateAdmin: %v", err)
	}
	if stats.Total24h != 0 {
		t.Errorf("Total24h should ignore doorbell_cancel rows; got %d", stats.Total24h)
	}
}

// ---------- Saison 14-04-Phase2 soft-delete + pagination ----------

func seedThreeEvents(t *testing.T, s *SQLStore) (ids [3]int64) {
	t.Helper()
	ctx := context.Background()
	base := time.Unix(1747000000, 0)
	for i, when := range []time.Time{
		base.Add(-2 * time.Hour),
		base.Add(-1 * time.Hour),
		base,
	} {
		id, err := s.Insert(ctx, Event{
			MockMAC:    testMockMAC,
			EventType:  TypeDoorbellStart,
			OccurredAt: when,
		}, nil)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids[i] = id
	}
	return ids
}

func TestHideEvent_HidesOnlySpecifiedID(t *testing.T) {
	s, _ := newStore(t)
	ids := seedThreeEvents(t, s)
	if err := s.HideEvent(context.Background(), testMockMAC, ids[1]); err != nil {
		t.Fatalf("HideEvent: %v", err)
	}
	events, err := s.ListVisible(context.Background(), testMockMAC, ListOpts{})
	if err != nil {
		t.Fatalf("ListVisible: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ListVisible len = %d, want 2", len(events))
	}
	for _, ev := range events {
		if ev.ID == ids[1] {
			t.Errorf("hidden event %d still visible", ids[1])
		}
	}
}

func TestHideEvent_IsIdempotent(t *testing.T) {
	s, _ := newStore(t)
	ids := seedThreeEvents(t, s)
	for i := 0; i < 3; i++ {
		if err := s.HideEvent(context.Background(), testMockMAC, ids[0]); err != nil {
			t.Fatalf("HideEvent iter %d: %v", i, err)
		}
	}
}

func TestHideEvent_OtherViewerCannotHide(t *testing.T) {
	s, _ := newStore(t)
	ids := seedThreeEvents(t, s)
	// macB tries to hide one of macA's events. The cross-viewer
	// hide must not succeed: HideEvent enforces the mock-scope via
	// a sub-select.
	err := s.HideEvent(context.Background(), testMockMACB, ids[0])
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-viewer hide returned %v, want ErrNotFound", err)
	}
	// And the event is still visible for macA.
	events, _ := s.ListVisible(context.Background(), testMockMAC, ListOpts{})
	if len(events) != 3 {
		t.Errorf("ListVisible after cross-viewer hide = %d, want 3", len(events))
	}
}

func TestHideAllEvents_HidesEverythingForMAC(t *testing.T) {
	s, _ := newStore(t)
	seedThreeEvents(t, s)
	n, err := s.HideAllEvents(context.Background(), testMockMAC)
	if err != nil {
		t.Fatalf("HideAllEvents: %v", err)
	}
	if n != 3 {
		t.Errorf("HideAllEvents returned %d, want 3", n)
	}
	events, _ := s.ListVisible(context.Background(), testMockMAC, ListOpts{})
	if len(events) != 0 {
		t.Errorf("ListVisible after HideAll = %d, want 0", len(events))
	}
	// Re-Aktivierung-Semantik: HideAllEvents bei einem leeren
	// sichtbaren Set sollte 0 neu-versteckte zurueckgeben.
	n2, err := s.HideAllEvents(context.Background(), testMockMAC)
	if err != nil {
		t.Fatalf("HideAllEvents second call: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second HideAllEvents returned %d, want 0", n2)
	}
}

func TestListVisible_Pagination(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	// 5 Events seeden (zeitlich gestaffelt).
	base := time.Unix(1747000000, 0)
	for i := 0; i < 5; i++ {
		if _, err := s.Insert(ctx, Event{
			MockMAC:    testMockMAC,
			EventType:  TypeDoorbellStart,
			OccurredAt: base.Add(time.Duration(i) * time.Hour),
		}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	page1, err := s.ListVisible(ctx, testMockMAC, ListOpts{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 {
		t.Errorf("page1 len = %d, want 2", len(page1))
	}
	page2, err := s.ListVisible(ctx, testMockMAC, ListOpts{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("page2 len = %d, want 2", len(page2))
	}
	page3, err := s.ListVisible(ctx, testMockMAC, ListOpts{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3) != 1 {
		t.Errorf("page3 len = %d, want 1 (only the oldest event left)", len(page3))
	}
	// Pages duerfen sich nicht ueberlappen.
	seen := map[int64]bool{}
	for _, pg := range [][]Event{page1, page2, page3} {
		for _, ev := range pg {
			if seen[ev.ID] {
				t.Errorf("ID %d appears in multiple pages", ev.ID)
			}
			seen[ev.ID] = true
		}
	}
}

func TestListVisible_LimitClampedToMax(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	base := time.Unix(1747000000, 0)
	for i := 0; i < ListOptsMaxLimit+10; i++ {
		if _, err := s.Insert(ctx, Event{
			MockMAC:    testMockMAC,
			EventType:  TypeDoorbellStart,
			OccurredAt: base.Add(time.Duration(i) * time.Second),
		}, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	got, err := s.ListVisible(ctx, testMockMAC, ListOpts{Limit: 9999})
	if err != nil {
		t.Fatalf("ListVisible: %v", err)
	}
	if len(got) != ListOptsMaxLimit {
		t.Errorf("limit=9999 returned %d rows, want clamp to %d",
			len(got), ListOptsMaxLimit)
	}
}

func TestListVisible_DateRange(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	day1 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	day3 := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	for _, t0 := range []time.Time{day1, day2, day3} {
		if _, err := s.Insert(ctx, Event{
			MockMAC:    testMockMAC,
			EventType:  TypeDoorbellStart,
			OccurredAt: t0,
		}, nil); err != nil {
			t.Fatalf("seed %v: %v", t0, err)
		}
	}
	// From=day2 schliesst day1 aus.
	got, err := s.ListVisible(ctx, testMockMAC, ListOpts{From: day2})
	if err != nil {
		t.Fatalf("ListVisible from: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("From=day2 returned %d, want 2 (day2+day3)", len(got))
	}
	// To=day2 schliesst day3 aus aber inkludiert beide bis Ende-Tag-23:59.
	got2, err := s.ListVisible(ctx, testMockMAC, ListOpts{To: day2})
	if err != nil {
		t.Fatalf("ListVisible to: %v", err)
	}
	if len(got2) != 2 {
		t.Errorf("To=day2 returned %d, want 2 (day1+day2 with end-of-day cutoff)", len(got2))
	}
	// From + To: nur day2 sichtbar.
	got3, err := s.ListVisible(ctx, testMockMAC, ListOpts{From: day2, To: day2})
	if err != nil {
		t.Fatalf("ListVisible range: %v", err)
	}
	if len(got3) != 1 {
		t.Errorf("From=To=day2 returned %d, want 1", len(got3))
	}
}

func TestCountVisible_IgnoresHidden(t *testing.T) {
	s, _ := newStore(t)
	ids := seedThreeEvents(t, s)
	if err := s.HideEvent(context.Background(), testMockMAC, ids[0]); err != nil {
		t.Fatalf("HideEvent: %v", err)
	}
	n, err := s.CountVisible(context.Background(), testMockMAC, ListOpts{})
	if err != nil {
		t.Fatalf("CountVisible: %v", err)
	}
	if n != 2 {
		t.Errorf("CountVisible after hide = %d, want 2", n)
	}
}

func TestHidden_SurvivesCascadeOnHardDelete(t *testing.T) {
	s, d := newStore(t)
	ids := seedThreeEvents(t, s)
	ctx := context.Background()
	if err := s.HideEvent(ctx, testMockMAC, ids[0]); err != nil {
		t.Fatalf("HideEvent: %v", err)
	}
	// Hard-delete des hidden Events. Cascade muss die hidden-Zeile
	// mitnehmen damit kein baumelnder FK liegt.
	if _, err := d.Exec(`DELETE FROM door_events WHERE id = ?`, ids[0]); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM viewer_hidden_events WHERE event_id = ?`, ids[0],
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("hidden row survived hard delete (count=%d)", n)
	}
}

// ---------- Admin reads + hard-delete ----------

func TestAdminListAll_IncludesHiddenWithFlag(t *testing.T) {
	s, _ := newStore(t)
	ids := seedThreeEvents(t, s)
	if err := s.HideEvent(context.Background(), testMockMAC, ids[0]); err != nil {
		t.Fatalf("HideEvent: %v", err)
	}
	res, err := s.AdminListAll(context.Background(), testMockMAC, ListOpts{})
	if err != nil {
		t.Fatalf("AdminListAll: %v", err)
	}
	if len(res.Events) != 3 {
		t.Fatalf("Events len = %d, want 3 (admin sees all)", len(res.Events))
	}
	if res.TotalCount != 3 {
		t.Errorf("TotalCount = %d, want 3", res.TotalCount)
	}
	if res.HiddenCount != 1 {
		t.Errorf("HiddenCount = %d, want 1", res.HiddenCount)
	}
	if res.HasMore {
		t.Errorf("HasMore = true, want false (3/3 fits in default page)")
	}
	var hiddenIdx = -1
	for i, ev := range res.Events {
		if ev.ID == ids[0] {
			hiddenIdx = i
			if !ev.HiddenByViewer {
				t.Errorf("hidden event %d HiddenByViewer = false", ids[0])
			}
			if ev.HiddenAt == nil {
				t.Errorf("hidden event %d HiddenAt nil", ids[0])
			}
		} else if ev.HiddenByViewer {
			t.Errorf("non-hidden event %d HiddenByViewer = true", ev.ID)
		}
	}
	if hiddenIdx < 0 {
		t.Errorf("hidden id %d not present in admin list", ids[0])
	}
}

func TestAdminListAll_PaginationHasMore(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	base := time.Unix(1747000000, 0)
	for i := 0; i < 5; i++ {
		if _, err := s.Insert(ctx, Event{
			MockMAC:    testMockMAC,
			EventType:  TypeDoorbellStart,
			OccurredAt: base.Add(time.Duration(i) * time.Hour),
		}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	res, err := s.AdminListAll(ctx, testMockMAC, ListOpts{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("AdminListAll: %v", err)
	}
	if !res.HasMore {
		t.Errorf("HasMore = false, want true (2 of 5)")
	}
	if res.TotalCount != 5 {
		t.Errorf("TotalCount = %d, want 5", res.TotalCount)
	}
	res2, err := s.AdminListAll(ctx, testMockMAC, ListOpts{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("AdminListAll page 3: %v", err)
	}
	if res2.HasMore {
		t.Errorf("last page HasMore = true, want false")
	}
}

func TestAdminDeleteEvent_HardDeletes(t *testing.T) {
	s, d := newStore(t)
	ids := seedThreeEvents(t, s)
	if err := s.AdminDeleteEvent(context.Background(), testMockMAC, ids[1]); err != nil {
		t.Fatalf("AdminDeleteEvent: %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM door_events WHERE id = ?`, ids[1]).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("event still in DB after AdminDeleteEvent")
	}
}

func TestAdminDeleteEvent_CrossMACRejected(t *testing.T) {
	s, _ := newStore(t)
	ids := seedThreeEvents(t, s)
	err := s.AdminDeleteEvent(context.Background(), testMockMACB, ids[0])
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-mac delete returned %v, want ErrNotFound", err)
	}
}

func TestAdminDeleteAllForViewer_PurgesEverything(t *testing.T) {
	s, d := newStore(t)
	seedThreeEvents(t, s)
	// Add a hidden marker so we can verify cascade.
	ids, _ := s.ListVisible(context.Background(), testMockMAC, ListOpts{})
	if err := s.HideEvent(context.Background(), testMockMAC, ids[0].ID); err != nil {
		t.Fatalf("seed hide: %v", err)
	}
	n, err := s.AdminDeleteAllForViewer(context.Background(), testMockMAC)
	if err != nil {
		t.Fatalf("AdminDeleteAllForViewer: %v", err)
	}
	if n != 3 {
		t.Errorf("AdminDeleteAllForViewer returned %d, want 3", n)
	}
	// Hidden-Markers are also gone via FK CASCADE.
	var hidden int
	if err := d.QueryRow(`SELECT COUNT(*) FROM viewer_hidden_events WHERE viewer_mac = ?`, testMockMAC).Scan(&hidden); err != nil {
		t.Fatalf("count hidden: %v", err)
	}
	if hidden != 0 {
		t.Errorf("hidden markers survived AdminDeleteAllForViewer (count=%d)", hidden)
	}
}
