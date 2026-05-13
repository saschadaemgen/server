package doorhistory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"unifix.local/server/internal/db"
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
		`INSERT INTO viewers (mac, name, service_port, type, username, created_at, updated_at) VALUES (?, ?, ?, 'web', ?, ?, ?)`,
		testMockMAC, "Familie Mueller 2OG", 8100, "user-a", now, now,
	); err != nil {
		t.Fatalf("seed viewer A: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, username, created_at, updated_at) VALUES (?, ?, ?, 'web', ?, ?, ?)`,
		testMockMACB, "Wohnung 2", 8101, "user-b", now, now,
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
