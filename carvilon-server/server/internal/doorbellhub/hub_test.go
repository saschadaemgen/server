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
	"carvilon.local/server/internal/fcm"
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

// fakeSource is the minimal stand-in for viewermanager.Manager.
// The hub does not need LookupUserByMAC.
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

	h.Publish(macA, Event{Type: TypeDoorbellStart, ViewerMAC: macA})

	select {
	case ev := <-subA.Events:
		if ev.ViewerMAC != macA {
			t.Errorf("subA got mac=%q, want %q", ev.ViewerMAC, macA)
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
		ViewerMAC:    macA,
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
		if ev.ViewerMAC != macA {
			t.Errorf("MockMAC = %q", ev.ViewerMAC)
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
		ViewerMAC:     macA,
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

	src.events <- mock.DoorbellEvent{ViewerMAC: "0c:ea:14:99:99:99"}

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

	src.events <- mock.DoorbellEvent{ViewerMAC: ""}

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

// ---------- History persistence ----------

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
		mockMAC:   ev.ViewerMAC,
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
		ViewerMAC:     macA,
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
		ViewerMAC:     macA,
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
		ViewerMAC:    macA,
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

// ---------- eventbus + doorbellcalls wires ----------

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
		ViewerMAC:     mac,
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
		ViewerMAC:     mac,
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

// ---------- config.changed ----------

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
		if ev.ViewerMAC != macA {
			t.Errorf("MockMAC = %q, want %q", ev.ViewerMAC, macA)
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
		ViewerMAC:     "mac-x",
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
		ViewerMAC:     "mac-x",
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

// ---------- FCM doorbell push leg (Saison 17) ----------

type fakeFCMTokens struct {
	token string
	err   error
}

func (f fakeFCMTokens) GetFCMToken(_ context.Context, _ string) (string, error) {
	return f.token, f.err
}

type recordingFCMSender struct {
	mu     sync.Mutex
	calls  int
	token  string
	push   fcm.DoorbellPush
	retErr error
	// cancel leg (Saison 19-20)
	cancelCalls int
	cancelToken string
	cancelPush  fcm.DoorbellPush
	// config.changed leg (Saison 19-34)
	configCalls     int
	configToken     string
	configViewerMAC string
}

func (r *recordingFCMSender) Send(_ context.Context, token string, push fcm.DoorbellPush) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.token = token
	r.push = push
	return r.retErr
}

func (r *recordingFCMSender) SendCancel(_ context.Context, token string, push fcm.DoorbellPush) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelCalls++
	r.cancelToken = token
	r.cancelPush = push
	return r.retErr
}

func (r *recordingFCMSender) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *recordingFCMSender) snapshot() (string, fcm.DoorbellPush) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.token, r.push
}

func (r *recordingFCMSender) cancelCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelCalls
}

func (r *recordingFCMSender) cancelSnapshot() (string, fcm.DoorbellPush) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelToken, r.cancelPush
}

func (r *recordingFCMSender) SendConfigChanged(_ context.Context, token, viewerMAC string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configCalls++
	r.configToken = token
	r.configViewerMAC = viewerMAC
	return r.retErr
}

func (r *recordingFCMSender) configCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.configCalls
}

func (r *recordingFCMSender) configSnapshot() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.configToken, r.configViewerMAC
}

// ---------- FCM config.changed wake-up leg (Saison 19-34) ----------

func TestBroadcastConfigChanged_FCMLeg_TokenPresentSends(t *testing.T) {
	src := newFakeSource()
	sender := &recordingFCMSender{}
	h := NewWithOptions(src, nil, quietLogger(), Options{
		FCMTokens: fakeFCMTokens{token: "phone-token-cfg"},
		FCMSender: sender,
	})

	h.BroadcastConfigChanged(context.Background(), "0c:ea:14:00:00:07")

	// SendConfigChanged runs in a detached goroutine; wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sender.configCallCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if sender.configCallCount() != 1 {
		t.Fatalf("config.changed FCM calls = %d, want 1", sender.configCallCount())
	}
	tok, mac := sender.configSnapshot()
	if tok != "phone-token-cfg" || mac != "0c:ea:14:00:00:07" {
		t.Errorf("config push = (%q, %q), want (phone-token-cfg, 0c:ea:14:00:00:07)", tok, mac)
	}
	// A config broadcast must NOT fire the doorbell ring/cancel legs.
	if sender.callCount() != 0 || sender.cancelCallCount() != 0 {
		t.Errorf("doorbell legs fired on config.changed: send=%d cancel=%d",
			sender.callCount(), sender.cancelCallCount())
	}
}

// web/esp viewers carry no fcm_token -> no FCM leg fires (path unchanged).
func TestBroadcastConfigChanged_FCMLeg_NoTokenNoSend(t *testing.T) {
	src := newFakeSource()
	sender := &recordingFCMSender{}
	h := NewWithOptions(src, nil, quietLogger(), Options{
		FCMTokens: fakeFCMTokens{token: ""}, // web/esp: no phone registered
		FCMSender: sender,
	})

	h.BroadcastConfigChanged(context.Background(), "0c:ea:14:00:00:08")

	// Give any (erroneous) goroutine a chance to fire before asserting none did.
	time.Sleep(100 * time.Millisecond)
	if sender.configCallCount() != 0 {
		t.Errorf("config.changed FCM calls = %d, want 0 (no token)", sender.configCallCount())
	}
}

func TestDispatchDoorbell_FCMLeg_TokenPresentSends(t *testing.T) {
	src := newFakeSource()
	sender := &recordingFCMSender{}
	h := NewWithOptions(src, nil, quietLogger(), Options{
		FCMTokens: fakeFCMTokens{token: "phone-token-1"},
		FCMSender: sender,
	})

	h.dispatchDoorbell(context.Background(), mock.DoorbellEvent{
		ViewerMAC:      "0c:ea:14:00:00:01",
		DeviceName:     "Hauseingang",
		RoomID:         "WR-room-9",
		CancelToken:    "cancel-9",
		CreateTimeUnix: 1747000000,
		ReceivedAt:     time.Unix(1747000005, 0),
	})

	// Send runs in a detached goroutine; wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sender.callCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if sender.callCount() != 1 {
		t.Fatalf("FCM Send call count = %d, want 1", sender.callCount())
	}
	token, push := sender.snapshot()
	if token != "phone-token-1" {
		t.Errorf("Send token = %q, want phone-token-1", token)
	}
	if push.StreamID != "0c:ea:14:00:00:01" || push.DeviceName != "Hauseingang" ||
		push.RoomID != "WR-room-9" || push.CancelToken != "cancel-9" || push.TS != "1747000000" {
		t.Errorf("push payload = %+v", push)
	}
}

func TestDispatchDoorbell_FCMLeg_NoTokenSkips(t *testing.T) {
	src := newFakeSource()
	sender := &recordingFCMSender{}
	h := NewWithOptions(src, nil, quietLogger(), Options{
		FCMTokens: fakeFCMTokens{token: ""}, // no phone registered
		FCMSender: sender,
	})
	h.dispatchDoorbell(context.Background(), mock.DoorbellEvent{ViewerMAC: "0c:ea:14:00:00:02"})

	time.Sleep(100 * time.Millisecond)
	if got := sender.callCount(); got != 0 {
		t.Errorf("FCM Send call count = %d, want 0 (empty token)", got)
	}
}

// TestDispatchDoorbell_FCMLeg_SendErrorDoesNotBreakDispatch proves the
// Grundregel: even when the FCM token reader errors AND the sender
// would error, dispatchDoorbell still drives the local legs (SSE
// broadcast to the subscriber) to completion.
func TestDispatchDoorbell_FCMLeg_SendErrorDoesNotBreakDispatch(t *testing.T) {
	src := newFakeSource()
	sender := &recordingFCMSender{retErr: errors.New("fcm down")}
	h := NewWithOptions(src, nil, quietLogger(), Options{
		FCMTokens: fakeFCMTokens{token: "phone-token-2"},
		FCMSender: sender,
	})
	sub, cleanup := h.Subscribe("0c:ea:14:00:00:03")
	defer cleanup()

	h.dispatchDoorbell(context.Background(), mock.DoorbellEvent{
		ViewerMAC:   "0c:ea:14:00:00:03",
		CancelToken: "c3",
		ReceivedAt:  time.Unix(1747000010, 0),
	})

	// The local SSE leg must have fired regardless of FCM.
	select {
	case ev := <-sub.Events:
		if ev.Type != TypeDoorbellStart {
			t.Errorf("first event type = %q, want %q", ev.Type, TypeDoorbellStart)
		}
	case <-time.After(time.Second):
		t.Fatal("local SSE broadcast did not fire (FCM error broke dispatch?)")
	}
}

// TestDispatchCancel_FCMLeg_TokenPresentSendsCancel mirrors the ring-leg test:
// a UA-side abort fires a doorbell_cancel push keyed on cancel_token with the
// lifecycle reason.
func TestDispatchCancel_FCMLeg_TokenPresentSendsCancel(t *testing.T) {
	src := newFakeSource()
	sender := &recordingFCMSender{}
	h := NewWithOptions(src, nil, quietLogger(), Options{
		FCMTokens: fakeFCMTokens{token: "phone-token-c"},
		FCMSender: sender,
	})

	h.dispatchCancel(context.Background(), mock.DoorbellCancelEvent{
		ViewerMAC:   "0c:ea:14:00:00:0a",
		CancelToken: "cancel-aa",
		ReceivedAt:  time.Unix(1747000020, 0),
	})

	// SendCancel runs in a detached goroutine; wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sender.cancelCallCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if sender.cancelCallCount() != 1 {
		t.Fatalf("FCM SendCancel call count = %d, want 1", sender.cancelCallCount())
	}
	token, push := sender.cancelSnapshot()
	if token != "phone-token-c" {
		t.Errorf("SendCancel token = %q, want phone-token-c", token)
	}
	if push.StreamID != "0c:ea:14:00:00:0a" || push.CancelToken != "cancel-aa" ||
		push.Reason != doorbellcalls.ReasonTimeout {
		t.Errorf("cancel push = %+v, want stream_id/cancel_token set + reason=%q",
			push, doorbellcalls.ReasonTimeout)
	}
}

func TestDispatchCancel_FCMLeg_NoTokenSkips(t *testing.T) {
	src := newFakeSource()
	sender := &recordingFCMSender{}
	h := NewWithOptions(src, nil, quietLogger(), Options{
		FCMTokens: fakeFCMTokens{token: ""}, // no phone registered
		FCMSender: sender,
	})
	h.dispatchCancel(context.Background(), mock.DoorbellCancelEvent{
		ViewerMAC:   "0c:ea:14:00:00:0b",
		CancelToken: "cancel-bb",
	})

	time.Sleep(100 * time.Millisecond)
	if got := sender.cancelCallCount(); got != 0 {
		t.Errorf("FCM SendCancel call count = %d, want 0 (empty token)", got)
	}
}
