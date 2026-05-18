package doorbellhub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"carvilon.local/mock"
	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/doorbellcalls"
	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/eventbus"
)

var errBoom = errors.New("history boom")

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

// fakeSource is the minimal stand-in for mockmanager.Manager.
// Saison 12-06: the hub no longer needs LookupUserByMAC.
type fakeSource struct {
	events  chan mock.DoorbellEvent
	cancels chan mock.DoorbellCancelEvent
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		events:  make(chan mock.DoorbellEvent, 4),
		cancels: make(chan mock.DoorbellCancelEvent, 4),
	}
}

func (f *fakeSource) Events() <-chan mock.DoorbellEvent        { return f.events }
func (f *fakeSource) Cancels() <-chan mock.DoorbellCancelEvent { return f.cancels }

const (
	macA = "0c:ea:14:42:42:42"
	macB = "0c:ea:14:42:42:43"
)

// ---------- Subscribe / Unsubscribe ----------

func TestSubscribe_AddRemoveBalance(t *testing.T) {
	src := newFakeSource()
	h := New(src, nil, quietLogger())
	_, cleanupA := h.Subscribe(macA)
	_, cleanupB := h.Subscribe(macB)
	_, cleanupC := h.Subscribe(macA)

	stats := h.Stats()
	if stats.SubscriberCount != 3 {
		t.Errorf("SubscriberCount = %d, want 3", stats.SubscriberCount)
	}
	if stats.UniqueMockCount != 2 {
		t.Errorf("UniqueMockCount = %d, want 2", stats.UniqueMockCount)
	}
	cleanupA()
	cleanupC()
	stats = h.Stats()
	if stats.SubscriberCount != 1 {
		t.Errorf("after partial cleanup SubscriberCount = %d, want 1", stats.SubscriberCount)
	}
	if stats.UniqueMockCount != 1 {
		t.Errorf("UniqueMockCount = %d, want 1", stats.UniqueMockCount)
	}
	cleanupB()
	stats = h.Stats()
	if stats.SubscriberCount != 0 || stats.UniqueMockCount != 0 {
		t.Errorf("after full cleanup = %+v, want zeros", stats)
	}
}

func TestCleanup_IsIdempotent(t *testing.T) {
	src := newFakeSource()
	h := New(src, nil, quietLogger())
	_, cleanup := h.Subscribe(macA)
	cleanup()
	cleanup()
	cleanup()
	if h.Stats().SubscriberCount != 0 {
		t.Error("repeated cleanup left subscribers")
	}
}

func TestCleanup_ClosesChannel(t *testing.T) {
	src := newFakeSource()
	h := New(src, nil, quietLogger())
	sub, cleanup := h.Subscribe(macA)
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

func TestPublish_BroadcastToMatchingMock(t *testing.T) {
	src := newFakeSource()
	h := New(src, nil, quietLogger())
	subA, cleanupA := h.Subscribe(macA)
	defer cleanupA()
	subB, cleanupB := h.Subscribe(macB)
	defer cleanupB()

	h.Publish(macA, Event{Type: TypeDoorbellStart, MockMAC: macA})

	select {
	case ev := <-subA.Events:
		if ev.MockMAC != macA {
			t.Errorf("subA got mac=%q, want %q", ev.MockMAC, macA)
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
	h := New(src, nil, quietLogger())
	sub, cleanup := h.Subscribe(macA)
	defer cleanup()
	for i := 0; i < subscriberBuffer; i++ {
		h.Publish(macA, Event{Type: TypeDoorbellStart})
	}
	if got := h.Stats().EventsDropped; got != 0 {
		t.Errorf("dropped before overflow = %d, want 0", got)
	}
	h.Publish(macA, Event{Type: TypeDoorbellStart})
	if got := h.Stats().EventsDropped; got != 1 {
		t.Errorf("dropped after overflow = %d, want 1", got)
	}
	for {
		select {
		case <-sub.Events:
		default:
			return
		}
	}
}

// ---------- Run loop dispatch ----------

func TestRun_DispatchesByMockMAC(t *testing.T) {
	src := newFakeSource()
	h := New(src, nil, quietLogger())
	sub, cleanup := h.Subscribe(macA)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.events <- mock.DoorbellEvent{
		MockMAC:    macA,
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
		if ev.MockMAC != macA {
			t.Errorf("MockMAC = %q", ev.MockMAC)
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

func TestRun_DispatchesCancelByMockMAC(t *testing.T) {
	src := newFakeSource()
	h := New(src, nil, quietLogger())
	sub, cleanup := h.Subscribe(macA)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.cancels <- mock.DoorbellCancelEvent{
		MockMAC:     macA,
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

func TestRun_NoSubscribersLogsAndDrops(t *testing.T) {
	src := newFakeSource()
	logger, buf := newLoggerWithCapture()
	h := New(src, nil, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.events <- mock.DoorbellEvent{MockMAC: "0c:ea:14:99:99:99"}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "no subscribers") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "no subscribers") {
		t.Errorf("expected no-subscribers log entry; got:\n%s", buf.String())
	}
}

func TestRun_EmptyMockMACDropped(t *testing.T) {
	src := newFakeSource()
	logger, buf := newLoggerWithCapture()
	h := New(src, nil, logger)
	sub, cleanup := h.Subscribe(macA)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.events <- mock.DoorbellEvent{MockMAC: ""}

	time.Sleep(50 * time.Millisecond)
	select {
	case ev := <-sub.Events:
		t.Errorf("got unexpected event %+v after empty mock_mac", ev)
	default:
	}
	if !strings.Contains(buf.String(), "without mock_mac") {
		t.Errorf("expected empty-mock-mac log entry; got:\n%s", buf.String())
	}
}

func TestRun_StopsCleanOnContextCancel(t *testing.T) {
	src := newFakeSource()
	h := New(src, nil, quietLogger())
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
	h := New(src, nil, quietLogger())
	_, cleanup := h.Subscribe(macA)
	defer cleanup()
	h.Publish(macA, Event{Type: TypeDoorbellStart})
	h.Publish(macA, Event{Type: TypeDoorbellCancel})
	if got := h.Stats().EventsTotal; got != 2 {
		t.Errorf("EventsTotal = %d, want 2", got)
	}
}

// ---------- History persistence (Saison 13-01) ----------

// fakeHistory is a minimal doorhistory.Store stub: it records
// Insert + UpdateCancel calls and exposes counters so tests can
// assert that the hub wrote to history before fanning out.
type fakeHistory struct {
	mu        sync.Mutex
	inserts   []fakeHistoryInsert
	cancels   []fakeHistoryCancel
	nextID    int64
	insertErr error
	cancelErr error
}

type fakeHistoryInsert struct {
	mockMAC   string
	eventType string
	cancelTok string
	rawLen    int
}

type fakeHistoryCancel struct {
	mockMAC   string
	cancelTok string
}

func (f *fakeHistory) Insert(_ context.Context, ev doorhistory.Event, raw []byte) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	f.nextID++
	f.inserts = append(f.inserts, fakeHistoryInsert{
		mockMAC:   ev.MockMAC,
		eventType: ev.EventType,
		cancelTok: ev.CancelToken,
		rawLen:    len(raw),
	})
	return f.nextID, nil
}

func (f *fakeHistory) UpdateCancel(_ context.Context, mockMAC, cancelTok string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancels = append(f.cancels, fakeHistoryCancel{mockMAC: mockMAC, cancelTok: cancelTok})
	return nil
}

func (f *fakeHistory) MarkRead(context.Context, string, []int64) error      { return nil }
func (f *fakeHistory) MarkAllRead(context.Context, string, time.Time) error { return nil }
func (f *fakeHistory) ListForMock(context.Context, string, int) ([]doorhistory.Event, error) {
	return nil, nil
}
func (f *fakeHistory) UnreadCount(context.Context, string) (int, error) { return 0, nil }
func (f *fakeHistory) ListRecent(context.Context, int, ...string) ([]doorhistory.Event, error) {
	return nil, nil
}
func (f *fakeHistory) CountSince(context.Context, time.Time) (int, error) { return 0, nil }
func (f *fakeHistory) AggregateAdmin(context.Context, time.Time) (doorhistory.AdminStats, error) {
	return doorhistory.AdminStats{}, nil
}
func (f *fakeHistory) HideEvent(context.Context, string, int64) error    { return nil }
func (f *fakeHistory) HideAllEvents(context.Context, string) (int, error) { return 0, nil }
func (f *fakeHistory) ListVisible(context.Context, string, doorhistory.ListOpts) ([]doorhistory.Event, error) {
	return nil, nil
}
func (f *fakeHistory) CountVisible(context.Context, string, doorhistory.ListOpts) (int, error) {
	return 0, nil
}
func (f *fakeHistory) AdminListAll(context.Context, string, doorhistory.ListOpts) (doorhistory.AdminListResult, error) {
	return doorhistory.AdminListResult{}, nil
}
func (f *fakeHistory) AdminDeleteEvent(context.Context, string, int64) error    { return nil }
func (f *fakeHistory) AdminDeleteAllForViewer(context.Context, string) (int, error) {
	return 0, nil
}

func TestRun_PersistsAndAssignsEventID(t *testing.T) {
	src := newFakeSource()
	hist := &fakeHistory{}
	h := New(src, hist, quietLogger())
	sub, cleanup := h.Subscribe(macA)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.events <- mock.DoorbellEvent{
		MockMAC:     macA,
		RequestID:   "req-1",
		DeviceID:    "0c:ea:14:11:11:11",
		CancelToken: "tok-abc",
		ReceivedAt:  time.Unix(1747000000, 0),
		RawBody:     []byte("frame"),
	}
	select {
	case ev := <-sub.Events:
		if ev.EventID == 0 {
			t.Error("EventID = 0, want a persisted id")
		}
		if ev.CancelToken != "tok-abc" {
			t.Errorf("CancelToken = %q", ev.CancelToken)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no SSE event received")
	}

	hist.mu.Lock()
	defer hist.mu.Unlock()
	if len(hist.inserts) != 1 {
		t.Fatalf("inserts = %d, want 1", len(hist.inserts))
	}
	if hist.inserts[0].cancelTok != "tok-abc" {
		t.Errorf("inserted cancel_token = %q", hist.inserts[0].cancelTok)
	}
	if hist.inserts[0].rawLen != 5 {
		t.Errorf("raw frame length = %d, want 5", hist.inserts[0].rawLen)
	}
}

func TestRun_CancelInvokesUpdateCancel(t *testing.T) {
	src := newFakeSource()
	hist := &fakeHistory{}
	h := New(src, hist, quietLogger())
	sub, cleanup := h.Subscribe(macA)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.cancels <- mock.DoorbellCancelEvent{
		MockMAC:     macA,
		CancelToken: "tok-xyz",
		ReceivedAt:  time.Unix(1747000050, 0),
	}
	select {
	case ev := <-sub.Events:
		if ev.Type != TypeDoorbellCancel {
			t.Errorf("Type = %q", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no cancel event received")
	}

	hist.mu.Lock()
	defer hist.mu.Unlock()
	if len(hist.cancels) != 1 {
		t.Fatalf("cancels = %d, want 1", len(hist.cancels))
	}
	if hist.cancels[0].cancelTok != "tok-xyz" {
		t.Errorf("cancel token = %q", hist.cancels[0].cancelTok)
	}
}

func TestRun_PersistFailureStillDispatches(t *testing.T) {
	src := newFakeSource()
	hist := &fakeHistory{insertErr: errBoom}
	h := New(src, hist, quietLogger())
	sub, cleanup := h.Subscribe(macA)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.events <- mock.DoorbellEvent{
		MockMAC:    macA,
		RequestID:  "req-fail",
		ReceivedAt: time.Unix(1747000000, 0),
	}
	select {
	case ev := <-sub.Events:
		if ev.EventID != 0 {
			t.Errorf("EventID after persist failure = %d, want 0", ev.EventID)
		}
		if ev.RequestID != "req-fail" {
			t.Errorf("RequestID = %q", ev.RequestID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("persist failure swallowed the SSE dispatch")
	}
}

// ---------- Saison 13-03: eventbus + doorbellcalls wires ----------

func TestHub_PublishesToEventBus(t *testing.T) {
	src := newFakeSource()
	bus := eventbus.New()
	mac := "0c:ea:14:42:42:42"
	ch := bus.Subscribe(mac)
	defer bus.Unsubscribe(mac, ch)

	h := NewWithOptions(src, nil, quietLogger(), Options{Bus: bus})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.events <- mock.DoorbellEvent{
		MockMAC:     mac,
		RequestID:   "req-1",
		DeviceID:    "intercom-1",
		CancelToken: "tok-abc",
		ReceivedAt:  time.Unix(1747000001, 0),
	}
	select {
	case ev := <-ch:
		if ev.Type != "doorbell.ring" {
			t.Errorf("ev.Type = %q, want doorbell.ring", ev.Type)
		}
		if !strings.Contains(ev.JSON, `"event_id":"tok-abc"`) {
			t.Errorf("ev.JSON missing event_id=tok-abc: %s", ev.JSON)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("eventbus subscriber did not receive doorbell.ring")
	}

	src.cancels <- mock.DoorbellCancelEvent{
		MockMAC:     mac,
		CancelToken: "tok-abc",
		ReceivedAt:  time.Unix(1747000005, 0),
	}
	select {
	case ev := <-ch:
		if ev.Type != "doorbell.cancel" {
			t.Errorf("ev.Type = %q, want doorbell.cancel", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("eventbus subscriber did not receive doorbell.cancel")
	}
}

// ---------- Saison 14-XX config.changed ----------

func TestBroadcastConfigChanged_DispatchesToSubscriber(t *testing.T) {
	src := newFakeSource()
	h := New(src, nil, quietLogger())
	sub, cleanup := h.Subscribe(macA)
	defer cleanup()

	h.BroadcastConfigChanged(context.Background(), macA)

	select {
	case ev := <-sub.Events:
		if ev.Type != TypeConfigChanged {
			t.Errorf("Type = %q, want %q", ev.Type, TypeConfigChanged)
		}
		if ev.MockMAC != macA {
			t.Errorf("MockMAC = %q, want %q", ev.MockMAC, macA)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("subscriber did not receive config.changed")
	}
}

func TestBroadcastConfigChanged_FilteredByViewerMAC(t *testing.T) {
	src := newFakeSource()
	h := New(src, nil, quietLogger())
	subA, cleanupA := h.Subscribe(macA)
	defer cleanupA()
	subB, cleanupB := h.Subscribe(macB)
	defer cleanupB()

	h.BroadcastConfigChanged(context.Background(), macA)

	select {
	case ev := <-subA.Events:
		if ev.Type != TypeConfigChanged {
			t.Errorf("subA got Type = %q", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("subA did not receive config.changed")
	}
	select {
	case ev := <-subB.Events:
		t.Errorf("cross-tenant leak: subB got %+v", ev)
	case <-time.After(80 * time.Millisecond):
		// expected: nothing for B
	}
}

func TestBroadcastConfigChanged_PublishesToBus(t *testing.T) {
	src := newFakeSource()
	bus := eventbus.New()
	mac := "0c:ea:14:42:42:42"
	ch := bus.Subscribe(mac)
	defer bus.Unsubscribe(mac, ch)

	h := NewWithOptions(src, nil, quietLogger(), Options{Bus: bus})
	h.BroadcastConfigChanged(context.Background(), mac)

	select {
	case ev := <-ch:
		if ev.Type != TypeConfigChanged {
			t.Errorf("ev.Type = %q, want %q", ev.Type, TypeConfigChanged)
		}
		if ev.JSON != "{}" {
			t.Errorf("ev.JSON = %q, want %q", ev.JSON, "{}")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("eventbus subscriber did not receive config.changed")
	}
}

func TestBroadcastConfigChanged_EmptyMACNoOp(t *testing.T) {
	src := newFakeSource()
	bus := eventbus.New()
	h := NewWithOptions(src, nil, quietLogger(), Options{Bus: bus})
	// Without a subscriber: just verify no panic and EventsTotal
	// stays at zero.
	h.BroadcastConfigChanged(context.Background(), "")
	if got := h.Stats().EventsTotal; got != 0 {
		t.Errorf("EventsTotal = %d, want 0 after empty broadcast", got)
	}
}

func TestHub_StartsAndEndsCallLifecycle(t *testing.T) {
	src := newFakeSource()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "calls.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer d.Close()
	calls := doorbellcalls.New(d.DB)

	h := NewWithOptions(src, nil, quietLogger(), Options{Calls: calls})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	src.events <- mock.DoorbellEvent{
		MockMAC:     "mac-x",
		CancelToken: "tok-life",
		DeviceID:    "intercom-x",
		ReceivedAt:  time.Unix(1747000010, 0),
	}
	// Allow the dispatcher goroutine to insert.
	time.Sleep(80 * time.Millisecond)
	c, err := calls.Get(ctx, "tok-life")
	if err != nil {
		t.Fatalf("Get after start: %v", err)
	}
	if c.ViewerMAC != "mac-x" || c.DeviceID != "intercom-x" {
		t.Errorf("call row = %+v", c)
	}

	src.cancels <- mock.DoorbellCancelEvent{
		MockMAC:     "mac-x",
		CancelToken: "tok-life",
		ReceivedAt:  time.Unix(1747000040, 0),
	}
	time.Sleep(80 * time.Millisecond)
	c, err = calls.Get(ctx, "tok-life")
	if err != nil {
		t.Fatalf("Get after cancel: %v", err)
	}
	if c.CancelReason != doorbellcalls.ReasonTimeout {
		t.Errorf("CancelReason = %q, want %q", c.CancelReason, doorbellcalls.ReasonTimeout)
	}
	if c.EndedAt == nil {
		t.Error("EndedAt is nil after cancel")
	}
}
