package logbuf

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestRingKeepsLastN(t *testing.T) {
	b := New(5)
	for i := 0; i < 8; i++ {
		b.Publish(Entry{Time: int64(i), Message: "m"})
	}
	got := b.Backlog()
	if len(got) != 5 {
		t.Fatalf("backlog len = %d, want 5", len(got))
	}
	if got[0].Time != 3 || got[4].Time != 7 {
		t.Fatalf("ring kept wrong window: first=%d last=%d, want 3..7", got[0].Time, got[4].Time)
	}
}

func TestSubscribeLiveAndCancel(t *testing.T) {
	b := New(10)
	ch, cancel := b.Subscribe(4)
	b.Publish(Entry{Message: "hello"})
	select {
	case e := <-ch:
		if e.Message != "hello" {
			t.Fatalf("got %q, want hello", e.Message)
		}
	default:
		t.Fatal("subscriber did not receive the published entry")
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed after cancel")
	}
	cancel() // second cancel must be a no-op
	b.Publish(Entry{Message: "after"})
}

// stripTime removes the time attr so two handler outputs taken moments
// apart compare equal.
func stripTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

// The tee must not alter what the wrapped handler writes: the same
// records through a plain handler and a teed handler produce identical
// output (the stdout/journald invariant).
func TestTeeLeavesInnerOutputUnchanged(t *testing.T) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: stripTime}
	var plain, teed bytes.Buffer
	emit := func(l *slog.Logger) {
		l.Info("boot", "port", 8080)
		l.With("component", "mqtt-broker").Warn("listener down", "err", "eof")
		l.WithGroup("req").Error("failed", "path", "/x")
	}
	emit(slog.New(slog.NewTextHandler(&plain, opts)))
	emit(slog.New(Tee(slog.NewTextHandler(&teed, opts), New(10))))
	if plain.String() != teed.String() {
		t.Fatalf("tee changed inner output:\nplain: %q\nteed:  %q", plain.String(), teed.String())
	}
}

func TestComponentAttrBecomesSubsystem(t *testing.T) {
	buf := New(10)
	log := slog.New(Tee(slog.NewTextHandler(&bytes.Buffer{}, nil), buf))
	log.With("component", "mqtt-broker").Info("broker started", "addr", ":1883")
	log.Info("inline", "component", "telegram-bot", "chat", 7)

	got := buf.Backlog()
	if len(got) != 2 {
		t.Fatalf("backlog len = %d, want 2", len(got))
	}
	if got[0].Subsystem != "mqtt-broker" {
		t.Fatalf("With subsystem = %q, want mqtt-broker", got[0].Subsystem)
	}
	if got[0].Message != "broker started addr=:1883" {
		t.Fatalf("message = %q (component must not repeat)", got[0].Message)
	}
	if got[1].Subsystem != "telegram-bot" || got[1].Message != "inline chat=7" {
		t.Fatalf("inline attr: sys=%q msg=%q", got[1].Subsystem, got[1].Message)
	}
	if got[0].Level != "INFO" {
		t.Fatalf("level = %q, want INFO", got[0].Level)
	}
}

func TestSubsystemFallsBackToCallerPackage(t *testing.T) {
	buf := New(10)
	log := slog.New(Tee(slog.NewTextHandler(&bytes.Buffer{}, nil), buf))
	log.Info("plain line")
	got := buf.Backlog()
	if len(got) != 1 || got[0].Subsystem != "logbuf" {
		t.Fatalf("subsystem = %q, want logbuf (caller package)", got[0].Subsystem)
	}
}

func TestLevelGateFollowsInner(t *testing.T) {
	buf := New(10)
	inner := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(Tee(inner, buf))
	log.Debug("invisible")
	log.Info("visible")
	got := buf.Backlog()
	if len(got) != 1 || got[0].Message != "visible" {
		t.Fatalf("buffer = %+v, want only the INFO line", got)
	}
}

func TestGroupsRenderDotted(t *testing.T) {
	buf := New(10)
	log := slog.New(Tee(slog.NewTextHandler(&bytes.Buffer{}, nil), buf))
	log.WithGroup("req").Info("handled", "path", "/x")
	log.Info("grouped", slog.Group("io", slog.Int("n", 3)))
	got := buf.Backlog()
	if len(got) != 2 {
		t.Fatalf("backlog len = %d, want 2", len(got))
	}
	if !strings.Contains(got[0].Message, "req.path=/x") {
		t.Fatalf("WithGroup message = %q, want req.path=/x", got[0].Message)
	}
	if !strings.Contains(got[1].Message, "io.n=3") {
		t.Fatalf("Group attr message = %q, want io.n=3", got[1].Message)
	}
}

func TestNilBufferIsPassThrough(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(Tee(slog.NewTextHandler(&out, &slog.HandlerOptions{ReplaceAttr: stripTime}), nil))
	log.Info("still works")
	if !strings.Contains(out.String(), "still works") {
		t.Fatalf("inner output missing: %q", out.String())
	}
}
