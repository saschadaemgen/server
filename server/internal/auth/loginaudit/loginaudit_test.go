package loginaudit

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return New(d)
}

func TestInsert_PersistsRow(t *testing.T) {
	s := newTestService(t)
	err := s.Insert(context.Background(), Entry{
		Realm:    RealmViewer,
		Username: "alice",
		IP:       "1.1.1.1",
		Outcome:  OutcomeSuccess,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.Recent(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Recent len = %d, want 1", len(got))
	}
	if got[0].Username != "alice" {
		t.Errorf("username = %q", got[0].Username)
	}
	if got[0].Outcome != OutcomeSuccess {
		t.Errorf("outcome = %q", got[0].Outcome)
	}
}

func TestInsert_RejectsEmptyOutcome(t *testing.T) {
	s := newTestService(t)
	err := s.Insert(context.Background(), Entry{Username: "alice"})
	if err == nil {
		t.Error("Insert without outcome should fail")
	}
}

func TestRecent_FiltersByRealm(t *testing.T) {
	s := newTestService(t)
	for _, e := range []Entry{
		{Realm: RealmViewer, Username: "alice", Outcome: OutcomeSuccess},
		{Realm: RealmAdmin, Username: "saschsa", Outcome: OutcomeSuccess},
		{Realm: RealmViewer, Username: "bob", Outcome: OutcomeFail},
	} {
		if err := s.Insert(context.Background(), e); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	v, _ := s.Recent(context.Background(), RealmViewer, 10)
	if len(v) != 2 {
		t.Errorf("RealmViewer entries = %d, want 2", len(v))
	}
	a, _ := s.Recent(context.Background(), RealmAdmin, 10)
	if len(a) != 1 {
		t.Errorf("RealmAdmin entries = %d, want 1", len(a))
	}
}

func TestRecent_OrderedNewestFirst(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()
	tick := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		t := tick
		tick = tick.Add(time.Second)
		return t
	}
	s := NewWithClock(d, clock)
	for _, name := range []string{"alice", "bob", "carol"} {
		if err := s.Insert(context.Background(), Entry{
			Realm:    RealmViewer,
			Username: name,
			Outcome:  OutcomeSuccess,
		}); err != nil {
			t.Fatalf("Insert %s: %v", name, err)
		}
	}
	got, _ := s.Recent(context.Background(), RealmViewer, 10)
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Username != "carol" || got[2].Username != "alice" {
		t.Errorf("ordering wrong: %v", []string{got[0].Username, got[1].Username, got[2].Username})
	}
}

func TestCleanup_RemovesAged(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	s := NewWithClock(d, clock)

	if err := s.Insert(context.Background(), Entry{
		Realm:     RealmViewer,
		Username:  "old",
		Timestamp: now.Add(-100 * 24 * time.Hour),
		Outcome:   OutcomeSuccess,
	}); err != nil {
		t.Fatalf("Insert old: %v", err)
	}
	if err := s.Insert(context.Background(), Entry{
		Realm:     RealmViewer,
		Username:  "new",
		Timestamp: now.Add(-1 * 24 * time.Hour),
		Outcome:   OutcomeSuccess,
	}); err != nil {
		t.Fatalf("Insert new: %v", err)
	}

	n, err := s.Cleanup(context.Background(), 90*24*time.Hour)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if n != 1 {
		t.Errorf("Cleanup deleted %d, want 1", n)
	}
	got, _ := s.Recent(context.Background(), RealmViewer, 10)
	if len(got) != 1 || got[0].Username != "new" {
		t.Errorf("survivor = %+v", got)
	}
}
