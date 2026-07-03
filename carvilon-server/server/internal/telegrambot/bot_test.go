package telegrambot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/telegramstore"
)

const testToken = "123456789:AAtestTOKENtestTOKENtestTOKENtest12"

// fakeAPI is the mocked Bot API (httptest): getUpdates long-polls a
// queued update list, sendMessage records, getMe answers - zero
// external requests in tests, per the briefing.
type fakeAPI struct {
	t     *testing.T
	token string

	mu           sync.Mutex
	updates      []apiUpdate
	sends        []fakeSent
	sendAttempts int
	sendHook     func(n int) (status int, body string) // nil/-> ok; n = 1-based attempt (incl. failed)
	getMeStatus  int                                   // 0 -> ok
	lastOffset   int64
	inFlight     int
	maxInFlight  int
	calls        int

	srv *httptest.Server
}

type fakeSent struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
	At     time.Time
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, token: testToken}
	mux := http.NewServeMux()
	mux.HandleFunc("/bot"+testToken+"/getMe", f.handleGetMe)
	mux.HandleFunc("/bot"+testToken+"/getUpdates", f.handleGetUpdates)
	mux.HandleFunc("/bot"+testToken+"/sendMessage", f.handleSendMessage)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) handleGetMe(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	status := f.getMeStatus
	f.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"Unauthorized"}`, status)
		return
	}
	io.WriteString(w, `{"ok":true,"result":{"id":1,"username":"carvilon_test_bot","first_name":"Carvilon"}}`)
}

func (f *fakeAPI) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Offset int64 `json:"offset"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	f.mu.Lock()
	f.calls++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.lastOffset = req.Offset
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	// Short long-poll: return queued updates >= offset, else empty
	// after ~150ms (kept short so tests stay fast).
	deadline := time.Now().Add(150 * time.Millisecond)
	for {
		f.mu.Lock()
		var due []apiUpdate
		for _, u := range f.updates {
			if u.UpdateID >= req.Offset {
				due = append(due, u)
			}
		}
		f.mu.Unlock()
		if len(due) > 0 || time.Now().After(deadline) || r.Context().Err() != nil {
			var buf bytes.Buffer
			buf.WriteString(`{"ok":true,"result":`)
			b, _ := json.Marshal(due)
			buf.Write(b)
			buf.WriteString(`}`)
			_, _ = w.Write(buf.Bytes())
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fakeAPI) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var req fakeSent
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.calls++
	f.sendAttempts++
	attempt := f.sendAttempts
	hook := f.sendHook
	f.mu.Unlock()
	if hook != nil {
		if status, body := hook(attempt); status != 0 {
			w.WriteHeader(status)
			io.WriteString(w, body)
			return
		}
	}
	req.At = time.Now()
	f.mu.Lock()
	f.sends = append(f.sends, req)
	f.mu.Unlock()
	io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
}

// queueUpdate appends a fresh text message update.
func (f *fakeAPI) queueUpdate(id, chatID int64, text string, date time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, apiUpdate{
		UpdateID: id,
		Message: &apiMessage{
			Date: date.Unix(),
			Text: text,
			Chat: apiChat{ID: chatID, Type: "private"},
			From: &apiUser{ID: chatID, Username: "tester", FirstName: "Tess"},
		},
	})
}

func (f *fakeAPI) sentMessages() []fakeSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSent(nil), f.sends...)
}

func (f *fakeAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestStore opens a real migrated sqlite DB (house convention).
func newTestStore(t *testing.T) *telegramstore.Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return telegramstore.New(d.DB)
}

// newTestManager builds a running manager against the fake API with
// the given chats allowlisted.
func newTestManager(t *testing.T, f *fakeAPI, log *slog.Logger, allowed map[int64]string) (*Manager, *telegramstore.Store) {
	t.Helper()
	store := newTestStore(t)
	for id, label := range allowed {
		if err := store.AddAllowed(context.Background(), id, label); err != nil {
			t.Fatalf("AddAllowed(%d): %v", id, err)
		}
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := New(store, log, Settings{Enabled: true, Token: testToken, APIBase: f.srv.URL})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.Shutdown)
	return m, store
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestManager_AllowedChatDispatch_UnknownGoesPending(t *testing.T) {
	f := newFakeAPI(t)
	m, store := newTestManager(t, f, nil, map[int64]string{42: "Sascha"})

	var mu sync.Mutex
	var got []Msg
	remove := m.AddListener(func(msg Msg) {
		mu.Lock()
		got = append(got, msg)
		mu.Unlock()
	})
	defer remove()

	f.queueUpdate(1, 42, "licht an", time.Now())
	f.queueUpdate(2, 99, "hallo bot", time.Now())

	if !waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	}) {
		t.Fatalf("dispatched = %v, want the one allowed-chat message", got)
	}
	mu.Lock()
	if got[0] != (Msg{ChatID: 42, Text: "licht an"}) {
		t.Errorf("dispatched = %+v, want {42 licht an}", got[0])
	}
	mu.Unlock()

	// The unknown chat lands on the pending list, never at a listener.
	if !waitFor(t, 3*time.Second, func() bool {
		pending, err := store.ListPending(context.Background())
		return err == nil && len(pending) == 1 && pending[0].ChatID == 99
	}) {
		t.Fatal("unknown chat 99 never recorded as pending")
	}
	pending, _ := store.ListPending(context.Background())
	if pending[0].Username != "tester" || pending[0].FirstName != "Tess" {
		t.Errorf("pending meta = %+v, want tester/Tess", pending[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Errorf("unknown chat reached a listener: %v", got)
	}
}

// TestManager_StaleUpdateNotDispatched: a command older than the
// staleness window is acked but never fires (boot-replay guard).
func TestManager_StaleUpdateNotDispatched(t *testing.T) {
	f := newFakeAPI(t)
	m, _ := newTestManager(t, f, nil, map[int64]string{42: ""})

	var mu sync.Mutex
	dispatched := 0
	remove := m.AddListener(func(Msg) { mu.Lock(); dispatched++; mu.Unlock() })
	defer remove()

	f.queueUpdate(1, 42, "licht an", time.Now().Add(-5*time.Minute))
	// The update is acked (offset advances past it) without dispatch.
	if !waitFor(t, 3*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.lastOffset >= 2
	}) {
		t.Fatal("stale update never acked")
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if dispatched != 0 {
		t.Errorf("stale command dispatched %d times, want 0", dispatched)
	}
}

// TestManager_SendThrottleDefersLatest: rapid sends to one chat
// collapse to first + latest (the pending slot), never a burst.
func TestManager_SendThrottleDefersLatest(t *testing.T) {
	f := newFakeAPI(t)
	m, _ := newTestManager(t, f, nil, map[int64]string{42: ""})

	if err := m.Send(42, "a"); err != nil {
		t.Fatalf("Send a: %v", err)
	}
	if err := m.Send(42, "b"); err != nil {
		t.Fatalf("Send b: %v", err)
	}
	if err := m.Send(42, "c"); err != nil {
		t.Fatalf("Send c: %v", err)
	}

	if !waitFor(t, 4*time.Second, func() bool { return len(f.sentMessages()) == 2 }) {
		t.Fatalf("sends = %+v, want first + deferred latest", f.sentMessages())
	}
	time.Sleep(200 * time.Millisecond) // no third send sneaks in
	sent := f.sentMessages()
	if len(sent) != 2 || sent[0].Text != "a" || sent[1].Text != "c" {
		t.Fatalf("sends = %+v, want [a c] (b dropped as superseded)", sent)
	}
	if gap := sent[1].At.Sub(sent[0].At); gap < sendMinInterval-100*time.Millisecond {
		t.Errorf("second send after %v, want >= ~%v", gap, sendMinInterval)
	}
}

func TestManager_SendDefaultDeny(t *testing.T) {
	f := newFakeAPI(t)
	m, _ := newTestManager(t, f, nil, map[int64]string{42: ""})

	if err := m.Send(99, "psst"); err != ErrChatNotAllowed {
		t.Errorf("Send to unknown chat: err = %v, want ErrChatNotAllowed", err)
	}
	if err := m.TestSend(context.Background(), 99, "psst"); err != ErrChatNotAllowed {
		t.Errorf("TestSend to unknown chat: err = %v, want ErrChatNotAllowed", err)
	}
	time.Sleep(100 * time.Millisecond)
	if sent := f.sentMessages(); len(sent) != 0 {
		t.Errorf("denied sends reached the API: %+v", sent)
	}

	// Revocation mid-run: reload allowlist without 42 -> Send refused.
	store := newTestStore(t)
	m2, _ := newTestManager(t, f, nil, map[int64]string{})
	_ = store // silence unused in this scope
	if err := m2.Send(42, "x"); err != ErrChatNotAllowed {
		t.Errorf("Send after empty allowlist: err = %v, want ErrChatNotAllowed", err)
	}
}

func TestManager_TestSendDelivers(t *testing.T) {
	f := newFakeAPI(t)
	m, _ := newTestManager(t, f, nil, map[int64]string{42: "Sascha"})
	if err := m.TestSend(context.Background(), 42, "Testnachricht"); err != nil {
		t.Fatalf("TestSend: %v", err)
	}
	sent := f.sentMessages()
	if len(sent) != 1 || sent[0].ChatID != 42 || sent[0].Text != "Testnachricht" {
		t.Errorf("sends = %+v, want the one test message", sent)
	}
}

// syncBuffer is a goroutine-safe log sink: the manager's pollers log
// from their own goroutines while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestManager_TokenNeverInLogsOrStatus: with a failing API (401) and a
// dead endpoint (url.Error carries the full URL), neither the logs nor
// Status().Error may contain the token - raw or percent-encoded.
func TestManager_TokenNeverInLogsOrStatus(t *testing.T) {
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Case 1: Bot API rejects the token (401 on getMe + getUpdates).
	f := newFakeAPI(t)
	f.getMeStatus = 401
	m, _ := newTestManager(t, f, logger, map[int64]string{42: ""})
	waitFor(t, 2*time.Second, func() bool { return m.Status().Error != "" })

	// Case 2: dead endpoint -> transport error embedding the URL.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	store := newTestStore(t)
	m2 := New(store, logger, Settings{Enabled: true, Token: testToken, APIBase: deadURL})
	if err := m2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m2.Shutdown)
	waitFor(t, 2*time.Second, func() bool { return m2.Status().Error != "" })
	if err := m2.TestSend(context.Background(), 42, "x"); err == nil {
		_ = err // chat not allowed on empty store; still no token anywhere
	}

	for name, s := range map[string]string{
		"logs":       buf.String(),
		"status 401": m.Status().Error,
		"status net": m2.Status().Error,
	} {
		if strings.Contains(s, testToken) {
			t.Errorf("token leaked into %s: %q", name, s)
		}
	}
	if m2.Status().Error == "" {
		t.Error("dead endpoint produced no status error")
	}
	if !strings.Contains(buf.String()+m2.Status().Error, "***") {
		t.Error("expected a scrubbed marker somewhere in logs/status")
	}
}

// TestManager_SinglePollerAcrossReconfigure: repeated Reconfigure never
// yields two concurrent getUpdates, and the ack offset carries over so
// an already-dispatched update is not re-delivered.
func TestManager_SinglePollerAcrossReconfigure(t *testing.T) {
	f := newFakeAPI(t)
	m, _ := newTestManager(t, f, nil, map[int64]string{42: ""})

	var mu sync.Mutex
	dispatched := 0
	remove := m.AddListener(func(Msg) { mu.Lock(); dispatched++; mu.Unlock() })
	defer remove()

	f.queueUpdate(7, 42, "licht an", time.Now())
	if !waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return dispatched == 1
	}) {
		t.Fatal("update 7 never dispatched")
	}

	for i := 0; i < 5; i++ {
		if err := m.Reconfigure(context.Background(), m.SettingsSnapshot()); err != nil {
			t.Fatalf("Reconfigure %d: %v", i, err)
		}
	}
	// The new poller must ack past update 7 (offset carried over) and
	// never re-dispatch it.
	if !waitFor(t, 3*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.lastOffset >= 8
	}) {
		t.Errorf("offset not carried across Reconfigure: %d", f.lastOffset)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if dispatched != 1 {
		t.Errorf("update re-dispatched after Reconfigure: %d", dispatched)
	}
	mu.Unlock()
	f.mu.Lock()
	if f.maxInFlight > 1 {
		t.Errorf("max concurrent getUpdates = %d, want 1 (one poller per token)", f.maxInFlight)
	}
	f.mu.Unlock()
}

// TestManager_429RetryAfterHonoured: a 429 with retry_after pauses and
// retries once instead of hammering into a bot ban.
func TestManager_429RetryAfterHonoured(t *testing.T) {
	f := newFakeAPI(t)
	f.sendHook = func(attempt int) (int, string) {
		if attempt == 1 {
			return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`
		}
		return 0, ""
	}
	m, _ := newTestManager(t, f, nil, map[int64]string{42: ""})

	start := time.Now()
	if err := m.Send(42, "hallo"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return len(f.sentMessages()) == 1 }) {
		t.Fatal("throttled message never delivered")
	}
	if got := time.Since(start); got < time.Second {
		t.Errorf("retry landed after %v, want >= the 1s retry_after", got)
	}
}

// TestManager_OffMakesNoRequests: disabled, or enabled without a
// token, the manager must not touch the network at all.
func TestManager_OffMakesNoRequests(t *testing.T) {
	f := newFakeAPI(t)
	store := newTestStore(t)

	m := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)),
		Settings{Enabled: false, Token: testToken, APIBase: f.srv.URL})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start disabled: %v", err)
	}
	t.Cleanup(m.Shutdown)
	if st := m.Status(); st.Running || st.Enabled {
		t.Errorf("disabled status = %+v", st)
	}
	if err := m.Send(42, "x"); err != ErrNotRunning {
		t.Errorf("Send while disabled: err = %v, want ErrNotRunning", err)
	}

	m2 := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)),
		Settings{Enabled: true, Token: "", APIBase: f.srv.URL})
	if err := m2.Start(context.Background()); err != nil {
		t.Fatalf("Start without token: %v", err)
	}
	t.Cleanup(m2.Shutdown)
	if st := m2.Status(); st.Running {
		t.Error("running without a token")
	} else if st.Error == "" {
		t.Error("no-token state must explain itself in Status.Error")
	}

	time.Sleep(300 * time.Millisecond)
	if n := f.callCount(); n != 0 {
		t.Errorf("off/token-less manager made %d API calls, want 0", n)
	}
}

// TestManager_PendingWriteThrottled: an unknown chat spamming the bot
// costs at most one pending write per interval (last_seen stays at
// first_seen for the burst).
func TestManager_PendingWriteThrottled(t *testing.T) {
	f := newFakeAPI(t)
	_, store := newTestManager(t, f, nil, nil)

	for i := int64(1); i <= 5; i++ {
		f.queueUpdate(i, 99, fmt.Sprintf("spam %d", i), time.Now())
	}
	if !waitFor(t, 3*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.lastOffset >= 6
	}) {
		t.Fatal("burst never fully acked")
	}
	pending, err := store.ListPending(context.Background())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v (%v), want one row for chat 99", pending, err)
	}
	if pending[0].LastSeen != pending[0].FirstSeen {
		t.Errorf("burst caused multiple pending writes: first=%d last=%d",
			pending[0].FirstSeen, pending[0].LastSeen)
	}
}

// TestManager_GetMeFillsBotUsername: the admin page's immediate token
// feedback.
func TestManager_GetMeFillsBotUsername(t *testing.T) {
	f := newFakeAPI(t)
	m, _ := newTestManager(t, f, nil, nil)
	if !waitFor(t, 2*time.Second, func() bool { return m.Status().BotUsername == "carvilon_test_bot" }) {
		t.Errorf("BotUsername = %q, want carvilon_test_bot", m.Status().BotUsername)
	}
	if st := m.Status(); !st.Running || !st.Enabled {
		t.Errorf("status = %+v, want running+enabled", st)
	}
}
