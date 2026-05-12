package doorbellhub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"unifix.local/mock"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// lockedBuffer is a goroutine-safe wrapper around bytes.Buffer.
// slog Handler writes from the dispatch goroutine while the test
// inspects the buffer from the main goroutine; bytes.Buffer
// itself is not safe under that pattern.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newLoggerWithCapture() (*slog.Logger, *lockedBuffer) {
	buf := &lockedBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// fakeSource is a minimal stand-in for mockmanager.Manager.
type fakeSource struct {
	events  chan mock.DoorbellEvent
	cancels chan mock.DoorbellCancelEvent

	mu       sync.Mutex
	bindings map[string]string
	lookups  int
	errFor   map[string]error
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		events:   make(chan mock.DoorbellEvent, 4),
		cancels:  make(chan mock.DoorbellCancelEvent, 4),
		bindings: make(map[string]string),
		errFor:   make(map[string]error),
	}
}

func (f *fakeSource) Events() <-chan mock.DoorbellEvent        { return f.events }
func (f *fakeSource) Cancels() <-chan mock.DoorbellCancelEvent { return f.cancels }
func (f *fakeSource) LookupUserByMAC(_ context.Context, mac string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if err, ok := f.errFor[mac]; ok {
		return "", err
	}
	if uid, ok := f.bindings[mac]; ok {
		return uid, nil
	}
	return "", errors.New("not bound")
}

func (f *fakeSource) bind(mac, uaUserID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindings[mac] = uaUserID
}

// ---------- Subscribe / Unsubscribe ----------

func TestSubscribe_AddRemoveBalance(t *testing.T) {
	src := newFakeSource()
	h := New(src, quietLogger())
	subA, cleanupA := h.Subscribe("ua-user-1")
	subB, cleanupB := h.Subscribe("ua-user-2")
	subC, cleanupC := h.Subscribe("ua-user-1")
	_ = subA
	_ = subB
	_ = subC

	stats := h.Stats()
	if stats.SubscriberCount != 3 {
		t.Errorf("SubscriberCount = %d, want 3", stats.SubscriberCount)
	}
	if stats.UniqueUserCount != 2 {
		t.Errorf("UniqueUserCount = %d, want 2", stats.UniqueUserCount)
	}
	cleanupA()
	cleanupC()
	stats = h.Stats()
	if stats.SubscriberCount != 1 {
		t.Errorf("after partial cleanup SubscriberCount = %d, want 1", stats.SubscriberCount)
	}
	if stats.UniqueUserCount != 1 {
		t.Errorf("UniqueUserCount = %d, want 1", stats.UniqueUserCount)
	}
	cleanupB()
	stats = h.Stats()
	if stats.SubscriberCount != 0 || stats.UniqueUserCount != 0 {
		t.Errorf("after full cleanup = %+v, want zeros", stats)
	}
}

func TestCleanup_IsIdempotent(t *testing.T) {
	src := newFakeSource()
	h := New(src, quietLogger())
	_, cleanup := h.Subscribe("u")
	cleanup()
	cleanup()
	cleanup()
	if h.Stats().SubscriberCount != 0 {
		t.Error("repeated cleanup left subscribers")
	}
}

func TestCleanup_ClosesChannel(t *testing.T) {
	src := newFakeSource()
	h := New(src, quietLogger())
	sub, cleanup := h.Subscribe("u")
	cleanup()
	select {
	case _, ok := <-sub.Events:
		if ok {
			t.Error("channel still open after cleanup")
		}
	case <-time.After(50 * time.Millisecond):
		t.Error("channel did not close")
	}
}

// ---------- Broadcast routing ----------

func TestPublish_BroadcastToMatchingUser(t *testing.T) {
	src := newFakeSource()
	h := New(src, quietLogger())
	subA, cleanupA := h.Subscribe("ua-user-1")
	defer cleanupA()
	subB, cleanupB := h.Subscribe("ua-user-2")
	defer cleanupB()

	h.Publish("ua-user-1", Event{Type: TypeDoorbellStart, MockMAC: "x"})

	select {
	case ev := <-subA.Events:
		if ev.MockMAC != "x" {
			t.Errorf("subA got mac=%q, want x", ev.MockMAC)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subA did not receive event")
	}
	select {
	case ev := <-subB.Events:
		t.Errorf("subB got unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublish_DroppedWhenChannelFull(t *testing.T) {
	src := newFakeSource()
	h := New(src, quietLogger())
	sub, cleanup := h.Subscribe("u")
	defer cleanup()
	// Channel buffer is 8; fill it then send one more.
	for i := 0; i < subscriberBuffer; i++ {
		h.Publish("u", Event{Type: TypeDoorbellStart})
	}
	if got := h.Stats().EventsDropped; got != 0 {
		t.Errorf("dropped before overflow = %d, want 0", got)
	}
	h.Publish("u", Event{Type: TypeDoorbellStart})
	if got := h.Stats().EventsDropped; got != 1 {
		t.Errorf("dropped after overflow = %d, want 1", got)
	}
	// Drain so cleanup does not block.
	for {
		select {
		case <-sub.Events:
		default:
			return
		}
	}
}

// ---------- Run loop dispatch ----------

func TestRun_DispatchesDoorbellEvent(t *testing.T) {
	src := newFakeSource()
	src.bind("0c:ea:14:42:42:42", "ua-user-1")
	h := New(src, quietLogger())
	sub, cleanup := h.Subscribe("ua-user-1")
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.events <- mock.DoorbellEvent{
		MockMAC:    "0c:ea:14:42:42:42",
		RequestID:  "req-1",
		DeviceID:   "0c:ea:14:11:11:11",
		RoomID:     "WR-x",
		ReceivedAt: time.Unix(1747000000, 0),
	}
	select {
	case ev := <-sub.Events:
		if ev.Type != TypeDoorbellStart {
			t.Errorf("Type = %q", ev.Type)
		}
		if ev.RequestID != "req-1" {
			t.Errorf("RequestID = %q", ev.RequestID)
		}
		if ev.CreatedAt != 1747000000_000 {
			t.Errorf("CreatedAt = %d", ev.CreatedAt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no event received")
	}
}

func TestRun_DispatchesCancelEvent(t *testing.T) {
	src := newFakeSource()
	src.bind("0c:ea:14:42:42:42", "u1")
	h := New(src, quietLogger())
	sub, cleanup := h.Subscribe("u1")
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.cancels <- mock.DoorbellCancelEvent{
		MockMAC:     "0c:ea:14:42:42:42",
		CancelToken: "tok-42",
		ReceivedAt:  time.Unix(1747000050, 0),
	}
	select {
	case ev := <-sub.Events:
		if ev.Type != TypeDoorbellCancel {
			t.Errorf("Type = %q, want doorbell_cancel", ev.Type)
		}
		if ev.CancelToken != "tok-42" {
			t.Errorf("CancelToken = %q", ev.CancelToken)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no cancel event received")
	}
}

func TestRun_UnassignedMockLogsAndDrops(t *testing.T) {
	src := newFakeSource()
	// no binding for this mac
	logger, buf := newLoggerWithCapture()
	h := New(src, logger)
	sub, cleanup := h.Subscribe("u1")
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.events <- mock.DoorbellEvent{MockMAC: "0c:ea:14:99:99:99"}

	// Poll the log buffer until the dispatch goroutine has run.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "unassigned mock") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case ev := <-sub.Events:
		t.Errorf("u1 got unexpected event for unassigned mac: %+v", ev)
	default:
	}
	if !strings.Contains(buf.String(), "unassigned mock") {
		t.Errorf("expected unassigned-mock log entry; got:\n%s", buf.String())
	}
}

func TestRun_StopsCleanOnContextCancel(t *testing.T) {
	src := newFakeSource()
	h := New(src, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// ---------- Stats ----------

func TestStats_CountersIncrementOnPublish(t *testing.T) {
	src := newFakeSource()
	h := New(src, quietLogger())
	_, cleanup := h.Subscribe("u1")
	defer cleanup()
	h.Publish("u1", Event{Type: TypeDoorbellStart})
	h.Publish("u1", Event{Type: TypeDoorbellCancel})
	if got := h.Stats().EventsTotal; got != 2 {
		t.Errorf("EventsTotal = %d, want 2", got)
	}
}
