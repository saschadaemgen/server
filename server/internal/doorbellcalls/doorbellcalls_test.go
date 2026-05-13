package doorbellcalls

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"unifix.local/server/internal/db"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d.DB)
}

func TestStart_Idempotent(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if err := s.Start(ctx, "evt-1", "mac-a", "dev-x"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := s.Start(ctx, "evt-1", "mac-b", "dev-y"); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	c, err := s.Get(ctx, "evt-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.ViewerMAC != "mac-a" {
		t.Errorf("ViewerMAC = %q, want mac-a (first writer wins)", c.ViewerMAC)
	}
	if c.DeviceID != "dev-x" {
		t.Errorf("DeviceID = %q, want dev-x", c.DeviceID)
	}
}

func TestMarkAnswered_CASBehavior(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if err := s.Start(ctx, "evt-2", "mac-a", "dev-x"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first, err := s.MarkAnswered(ctx, "evt-2", "mac-a")
	if err != nil {
		t.Fatalf("first MarkAnswered: %v", err)
	}
	if !first {
		t.Error("first MarkAnswered returned false")
	}
	second, err := s.MarkAnswered(ctx, "evt-2", "mac-b")
	if err != nil {
		t.Fatalf("second MarkAnswered: %v", err)
	}
	if second {
		t.Error("second MarkAnswered returned true (race lost should be false)")
	}
	c, err := s.Get(ctx, "evt-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.AnsweredBy != "mac-a" {
		t.Errorf("AnsweredBy = %q, want mac-a", c.AnsweredBy)
	}
}

func TestMarkAnswered_UnknownEventReturnsNotFound(t *testing.T) {
	s := newTestService(t)
	first, err := s.MarkAnswered(context.Background(), "ghost", "mac-x")
	if !errors.Is(err, ErrCallNotFound) {
		t.Errorf("err = %v, want ErrCallNotFound", err)
	}
	if first {
		t.Error("first = true for unknown event")
	}
}

func TestMarkAnswered_AfterEnd_ReturnsFalseNoError(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if err := s.Start(ctx, "evt-3", "mac-a", ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.MarkEnded(ctx, "evt-3", "", ReasonTimeout); err != nil {
		t.Fatalf("MarkEnded: %v", err)
	}
	first, err := s.MarkAnswered(ctx, "evt-3", "mac-a")
	if err != nil {
		t.Fatalf("MarkAnswered: %v", err)
	}
	if first {
		t.Error("first = true for already-ended call")
	}
}

func TestMarkAnswered_MultiAnswerRace(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if err := s.Start(ctx, "evt-race", "mac-a", ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	const N = 20
	results := make(chan bool, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			first, _ := s.MarkAnswered(ctx, "evt-race", "mac-x")
			results <- first
		}(i)
	}
	wg.Wait()
	close(results)
	wins := 0
	for r := range results {
		if r {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("wins = %d, want exactly 1", wins)
	}
}

func TestMarkRejected_AndMarkEnded_AreIdempotent(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if err := s.Start(ctx, "evt-4", "mac-a", ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.MarkRejected(ctx, "evt-4", "mac-a"); err != nil {
		t.Fatalf("first MarkRejected: %v", err)
	}
	// A late timeout from doorbellhub must not overwrite the
	// real reason.
	if err := s.MarkEnded(ctx, "evt-4", "", ReasonTimeout); err != nil {
		t.Fatalf("late MarkEnded: %v", err)
	}
	c, err := s.Get(ctx, "evt-4")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.CancelReason != ReasonRejected {
		t.Errorf("CancelReason = %q, want %q (idempotent)", c.CancelReason, ReasonRejected)
	}
	if c.EndedBy != "mac-a" {
		t.Errorf("EndedBy = %q, want mac-a", c.EndedBy)
	}
}

func TestGetActive_ReturnsOnlyOpenCalls(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	_ = s.Start(ctx, "evt-open", "mac-a", "")
	_ = s.Start(ctx, "evt-closed", "mac-a", "")
	_ = s.MarkEnded(ctx, "evt-closed", "mac-a", ReasonUserEnded)
	active, err := s.GetActive(ctx, "mac-a")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if len(active) != 1 || active[0].EventID != "evt-open" {
		t.Errorf("active = %+v", active)
	}
}
