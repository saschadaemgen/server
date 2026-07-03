// Package telegrambot runs the embedded Telegram bot as an on/off
// Carvilon subsystem: ONE long-poll goroutine per token (getUpdates -
// Telegram allows a single consumer), a rate-limited send worker, and
// the chat allowlist as the default-deny gate for both directions.
//
// This is the platform's first and deliberately fenced-in cloud
// function: the only outbound target is api.telegram.org (or the
// injected test base URL), it is default-off, and a Telegram outage
// must never touch the engine tick, a running graph, or any other
// driver. The bot token is a secret - it is never logged and never
// rendered back into a page; every Bot-API error is sanitized at the
// client boundary (see api.go).
//
// Locking discipline (the mqtt-deadlock lesson, sharpened): mu guards
// the lifecycle only (settings, poller start/stop/join). stateMu
// guards the hot state that the engine's tick path reads through
// Send (allowlist, rate stamps, listeners, status text) and is NEVER
// held across network I/O, a store call, or the poller join. Listener
// callbacks route into the engine's tick queue (EnqueueInput takes
// the engine lock), so dispatch happens strictly OUTSIDE stateMu -
// holding it there would close an ABBA cycle with the tick's
// Write -> Send path. Lock order where both are needed: mu, then
// stateMu; never the reverse.
package telegrambot

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"carvilon.local/server/internal/telegramstore"
)

const (
	// sendMinInterval is the per-chat send throttle (~1 msg/s is
	// Telegram's per-chat guidance). A message arriving inside the
	// cooldown is parked in the chat's single pending slot and flushed
	// when the cooldown expires; only intermediate messages are dropped
	// - so a doorbell AND an alarm to the same chat both arrive, while
	// a fluttering trigger collapses to first + latest.
	sendMinInterval = 1100 * time.Millisecond
	// staleWindow: getUpdates redelivers up to ~24h of unconfirmed
	// updates (server downtime, poller restart). A command must never
	// fire long after it was typed - stale messages are acked but not
	// dispatched. They still surface unknown chats on the pending list.
	staleWindow = 60 * time.Second
	// pendingWriteInterval throttles pending-table writes per chat: an
	// unknown chat spamming the bot costs at most one SQLite write per
	// interval, not one per message.
	pendingWriteInterval = 60 * time.Second
	// outboxCap bounds the send queue; Send drops (never blocks) when
	// it is full - the engine tick must not feel Telegram backpressure.
	outboxCap = 64
	// errBackoffMax caps the poller's exponential error backoff; token
	// errors (401/404) and a competing poller (409) park at the cap
	// immediately so a misconfiguration never hammers the API.
	errBackoffMax = 30 * time.Second
)

// Sentinel errors for the send path (the driver logs them at debug).
var (
	ErrNotRunning     = errors.New("telegrambot: bot is not running")
	ErrChatNotAllowed = errors.New("telegrambot: chat is not on the allowlist")
	ErrQueueFull      = errors.New("telegrambot: send queue full")
)

// Settings are the runtime-tunable bot parameters. They are persisted
// by the admin layer (platform_config; the token as a secret) and
// handed to the Manager; the Manager itself never touches config
// storage.
type Settings struct {
	Enabled bool
	// Token is the BotFather token. Empty means "not set": the bot
	// stays down and the status says so. It lives only in memory here.
	Token string
	// APIBase overrides the Bot-API endpoint for tests (httptest).
	// Empty means the one permitted production target, api.telegram.org.
	APIBase string
}

// Status is a read snapshot for the admin UI and the catalog gate.
// Running means the poll loop is up (enabled + token set + started) -
// a boot-time fact like the MQTT listener bind, NOT current cloud
// reachability: a transient api.telegram.org outage or an invalid
// token surfaces in Error while Running stays true, so the palette
// category and graph binds do not flap with the network.
type Status struct {
	Enabled     bool
	Running     bool
	BotUsername string // from getMe, once known ("" until then)
	Error       string // last sanitized poll/send error ("" = healthy)
}

// Msg is one accepted incoming message: already allowlist-filtered and
// freshness-checked by the manager. The driver routes it to its bound
// command/text channels.
type Msg struct {
	ChatID int64
	Text   string
}

// Conn is the surface the telegram: engine driver binds to - the
// in-process equivalent of the MQTT driver's InlineClient. Send only
// stages into the rate-limited worker queue and never blocks (it is
// called from inside an engine tick). AddListener registers a
// callback for accepted incoming messages; the remove func
// unregisters it on run teardown.
type Conn interface {
	Send(chatID int64, text string) error
	AddListener(cb func(Msg)) (remove func())
}

type outMsg struct {
	chatID int64
	text   string
}

// Manager owns the bot lifecycle: the single poller, the send worker,
// and the in-memory allowlist snapshot.
type Manager struct {
	store *telegramstore.Store
	log   *slog.Logger

	// mu: lifecycle only (Start/Stop/Reconfigure). stopLocked joins the
	// poller under mu - safe because the poller never takes mu.
	mu       sync.Mutex
	settings Settings
	cancel   context.CancelFunc
	pollDone chan struct{}

	// offset is the getUpdates ack cursor. Atomic so the poller and a
	// Reconfigure handover never meet on a mutex; it survives poller
	// restarts (same token) and resets when the token changes.
	offset atomic.Int64

	// stateMu: hot, cheap state only - read on the engine tick path
	// (Send) and by the poller. NEVER held across network I/O, store
	// calls, the poller join, or a listener callback.
	stateMu     sync.Mutex
	running     bool
	api         *apiClient
	allowed     map[int64]string // chat id -> label
	lastSent    map[int64]time.Time
	listeners   map[int]func(Msg)
	nextLID     int
	pendingSeen map[int64]time.Time
	botUser     string
	lastErr     string

	outbox     chan outMsg
	workerQuit chan struct{}
	quitOnce   sync.Once
}

// New builds a Manager and starts its send worker. The poller starts
// with Start/Reconfigure when enabled and a token is set.
func New(store *telegramstore.Store, log *slog.Logger, settings Settings) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		store:       store,
		log:         log.With("component", "telegram-bot"),
		settings:    settings,
		allowed:     map[int64]string{},
		lastSent:    map[int64]time.Time{},
		listeners:   map[int]func(Msg){},
		pendingSeen: map[int64]time.Time{},
		outbox:      make(chan outMsg, outboxCap),
		workerQuit:  make(chan struct{}),
	}
	go m.sendLoop()
	return m
}

// SettingsSnapshot returns the current settings (including the token -
// for the admin save path to carry unchanged values; it must never
// reach a template).
func (m *Manager) SettingsSnapshot() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

// Status returns a snapshot for the admin UI.
func (m *Manager) Status() Status {
	m.mu.Lock()
	enabled := m.settings.Enabled
	m.mu.Unlock()
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return Status{Enabled: enabled, Running: m.running, BotUsername: m.botUser, Error: m.lastErr}
}

// Start brings the poller up if enabled and a token is set. A disabled
// bot, or one without a token, is a no-op success with the status
// explaining why. Called once at boot (non-fatal on error).
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(ctx)
}

// Stop tears the poller down and waits for it to exit (context-abort
// makes the join a matter of milliseconds). Safe when not running.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

// Shutdown stops the poller AND the send worker. Tests use it for
// goroutine hygiene; the production process just exits.
func (m *Manager) Shutdown() {
	m.Stop()
	m.quitOnce.Do(func() { close(m.workerQuit) })
}

// Reconfigure applies new settings: stop (join!) the old poller, then
// start the new one - the one-poller-per-token guarantee holds across
// the handover. The ack offset carries over for an unchanged token so
// already-dispatched updates are not re-fetched; a new token is a new
// bot with its own update stream, so the cursor resets.
func (m *Manager) Reconfigure(ctx context.Context, s Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
	if s.Token != m.settings.Token {
		m.offset.Store(0)
	}
	m.settings = s
	return m.startLocked(ctx)
}

// ReloadAllowlist swaps in a fresh allowlist snapshot. Called after
// every admin chat mutation (approve/reject/add/remove), so a revoked
// chat stops triggering commands and receiving messages mid-run.
func (m *Manager) ReloadAllowlist(ctx context.Context) error {
	allow, err := m.store.LoadAllowlist(ctx)
	if err != nil {
		return err
	}
	m.stateMu.Lock()
	m.allowed = allow
	m.stateMu.Unlock()
	return nil
}

// AllowedChats returns a copy of the allowlist snapshot (chat id ->
// label), for bind-time validation of send:/chat: channels.
func (m *Manager) AllowedChats() map[int64]string {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	out := make(map[int64]string, len(m.allowed))
	for id, l := range m.allowed {
		out[id] = l
	}
	return out
}

func (m *Manager) startLocked(ctx context.Context) error {
	m.setErr("")
	if !m.settings.Enabled {
		return nil
	}
	if m.settings.Token == "" {
		m.setErr("Bot-Token fehlt – auf /a/telegram setzen.")
		m.log.Warn("telegram bot enabled but no token set")
		return nil // a config state, not a failure
	}
	allow, err := m.store.LoadAllowlist(ctx)
	if err != nil {
		m.setErr("Allowlist laden: " + err.Error())
		return err
	}
	api := newAPIClient(m.settings.APIBase, m.settings.Token)
	// The poller's context derives from Background, NOT from ctx: a
	// Reconfigure passes the admin request's context, and the poller
	// must outlive the request. Shutdown happens via Stop (main defer).
	pctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.cancel, m.pollDone = cancel, done

	m.stateMu.Lock()
	m.allowed = allow
	m.api = api
	m.running = true
	m.stateMu.Unlock()

	go m.pollLoop(pctx, api, done)
	m.log.Info("telegram bot started", "allowed_chats", len(allow))
	return nil
}

func (m *Manager) stopLocked() {
	if m.cancel != nil {
		m.cancel()
		<-m.pollDone // in-flight getUpdates aborts via context; ms join
		m.cancel, m.pollDone = nil, nil
		// The started counterpart, so the lifecycle is visible in the
		// server log (and the designer's System Log tab).
		m.log.Info("telegram bot stopped")
	}
	m.stateMu.Lock()
	m.running = false
	m.api = nil
	m.botUser = ""
	m.stateMu.Unlock()
}

// ---- incoming: the one poller ----

func (m *Manager) pollLoop(ctx context.Context, api *apiClient, done chan struct{}) {
	defer close(done)
	// getMe first: immediate token feedback on the admin page (the bot
	// name), without stopping the loop on failure - the backoff below
	// keeps a misconfigured token from hammering the API.
	if u, err := api.getMe(ctx); err == nil {
		m.stateMu.Lock()
		m.botUser = u.Username
		m.stateMu.Unlock()
	} else if ctx.Err() == nil {
		m.setErr(err.Error())
	}

	backoff := time.Second
	for ctx.Err() == nil {
		ups, err := api.getUpdates(ctx, m.offset.Load())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.setErr(err.Error())
			m.log.Debug("telegram getUpdates failed", "err", err)
			wait := backoff
			if ae, ok := asAPIError(err); ok {
				switch ae.Code {
				case 401, 403, 404, 409:
					// Invalid token or a competing getUpdates consumer:
					// retrying fast helps nothing - park at the cap.
					wait = errBackoffMax
				}
			}
			if !sleepCtx(ctx, wait) {
				return
			}
			if backoff < errBackoffMax {
				backoff *= 2
				if backoff > errBackoffMax {
					backoff = errBackoffMax
				}
			}
			continue
		}
		backoff = time.Second
		m.setErr("")
		for _, u := range ups {
			m.offset.Store(u.UpdateID + 1) // ack: next poll confirms this one
			m.handleUpdate(ctx, u)
		}
	}
}

// handleUpdate applies the allowlist gate and freshness window, then
// dispatches to the driver listeners (outside stateMu - see the
// package comment on lock order) or records the unknown chat as
// pending (write-throttled per chat).
func (m *Manager) handleUpdate(ctx context.Context, u apiUpdate) {
	msg := u.Message
	if msg == nil || msg.Chat.ID == 0 {
		return
	}
	fresh := msg.Date > 0 && time.Since(time.Unix(msg.Date, 0)) <= staleWindow

	m.stateMu.Lock()
	_, allowed := m.allowed[msg.Chat.ID]
	var cbs []func(Msg)
	if allowed && fresh && msg.Text != "" {
		cbs = make([]func(Msg), 0, len(m.listeners))
		for _, cb := range m.listeners {
			cbs = append(cbs, cb)
		}
	}
	recordPending := false
	if !allowed {
		if last, ok := m.pendingSeen[msg.Chat.ID]; !ok || time.Since(last) >= pendingWriteInterval {
			m.pendingSeen[msg.Chat.ID] = time.Now()
			recordPending = true
		}
		// Bound the throttle map itself: drop stale entries once it
		// grows past any plausible legitimate size.
		if len(m.pendingSeen) > 1024 {
			for id, ts := range m.pendingSeen {
				if time.Since(ts) >= pendingWriteInterval {
					delete(m.pendingSeen, id)
				}
			}
		}
	}
	m.stateMu.Unlock()

	dm := Msg{ChatID: msg.Chat.ID, Text: msg.Text}
	for _, cb := range cbs {
		cb(dm)
	}

	if recordPending {
		var user, first string
		if msg.From != nil {
			user, first = msg.From.Username, msg.From.FirstName
		}
		if err := m.store.UpsertPending(ctx, msg.Chat.ID, user, first); err != nil {
			m.log.Debug("telegram pending upsert failed", "err", err)
		}
	}
}

// AddListener registers a callback for accepted incoming messages
// (part of Conn; the driver holds the remove func until Close).
func (m *Manager) AddListener(cb func(Msg)) (remove func()) {
	m.stateMu.Lock()
	id := m.nextLID
	m.nextLID++
	m.listeners[id] = cb
	m.stateMu.Unlock()
	return func() {
		m.stateMu.Lock()
		delete(m.listeners, id)
		m.stateMu.Unlock()
	}
}

// ---- outgoing: non-blocking enqueue + rate-limited worker ----

// Send stages a message for delivery (part of Conn). It is called
// from inside an engine tick, so it only checks the cheap state and
// enqueues - never a lock held across I/O, never blocking. Default
// deny outgoing too: a chat off the allowlist is refused even though
// the editor's picker should never produce one.
func (m *Manager) Send(chatID int64, text string) error {
	if text == "" {
		return nil // Telegram rejects empty text; nothing to say
	}
	m.stateMu.Lock()
	running := m.running
	_, ok := m.allowed[chatID]
	m.stateMu.Unlock()
	if !running {
		return ErrNotRunning
	}
	if !ok {
		return ErrChatNotAllowed
	}
	select {
	case m.outbox <- outMsg{chatID: chatID, text: text}:
		return nil
	default:
		return ErrQueueFull
	}
}

// TestSend delivers one message synchronously (the admin page's test
// button needs immediate feedback). It enforces the allowlist
// server-side - the form's chat select is client-side convenience,
// not the gate - and stamps the shared per-chat throttle.
func (m *Manager) TestSend(ctx context.Context, chatID int64, text string) error {
	m.stateMu.Lock()
	api := m.api
	running := m.running
	_, ok := m.allowed[chatID]
	if running && ok {
		m.lastSent[chatID] = time.Now()
	}
	m.stateMu.Unlock()
	if !running || api == nil {
		return ErrNotRunning
	}
	if !ok {
		return ErrChatNotAllowed
	}
	if err := api.sendMessage(ctx, chatID, text); err != nil {
		m.setErr(err.Error())
		return err // already sanitized at the client boundary
	}
	return nil
}

// sendLoop is the one worker draining the outbox. Per chat it keeps a
// minimum send spacing; a message inside the cooldown parks in that
// chat's single pending slot (latest wins, overwritten intermediates
// are dropped with a debug log) and flushes when the cooldown expires.
// All HTTP happens here, off the tick goroutine, no locks held.
func (m *Manager) sendLoop() {
	pending := map[int64]outMsg{}
	for {
		var wake <-chan time.Time
		if len(pending) > 0 {
			wake = time.After(m.nextFlushIn(pending))
		}
		select {
		case <-m.workerQuit:
			return
		case msg := <-m.outbox:
			if m.stampIfDue(msg.chatID) {
				// A newer message supersedes anything parked for the
				// chat - without this, a stale parked message would be
				// resurrected AFTER this one at the next flush.
				delete(pending, msg.chatID)
				m.deliver(msg)
			} else {
				if old, dup := pending[msg.chatID]; dup {
					m.log.Debug("telegram send throttled; dropping superseded message", "chat", old.chatID)
				}
				pending[msg.chatID] = msg
			}
		case <-wake:
			for id, msg := range pending {
				if m.stampIfDue(id) {
					delete(pending, id)
					m.deliver(msg)
				}
			}
		}
	}
}

// stampIfDue checks the per-chat cooldown and stamps it when clear.
// Shared with TestSend via lastSent under stateMu.
func (m *Manager) stampIfDue(chatID int64) bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if time.Since(m.lastSent[chatID]) < sendMinInterval {
		return false
	}
	m.lastSent[chatID] = time.Now()
	return true
}

// nextFlushIn computes how long until the earliest pending chat's
// cooldown expires (floor 50ms so a due flush never busy-loops).
func (m *Manager) nextFlushIn(pending map[int64]outMsg) time.Duration {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	next := time.Duration(1<<62 - 1)
	for id := range pending {
		d := sendMinInterval - time.Since(m.lastSent[id])
		if d < next {
			next = d
		}
	}
	if next < 50*time.Millisecond {
		next = 50 * time.Millisecond
	}
	return next
}

// deliver performs the actual sendMessage, honouring a 429's
// retry_after with one retry. Errors are logged (sanitized) and the
// message dropped - no crash, no unbounded buffering, and the engine
// never feels it. The allowlist is re-checked here so a chat revoked
// while its message sat in the queue (or the throttle slot) receives
// nothing - Send's check only covers enqueue time.
func (m *Manager) deliver(msg outMsg) {
	m.stateMu.Lock()
	api := m.api
	_, allowed := m.allowed[msg.chatID]
	m.stateMu.Unlock()
	if api == nil || !allowed {
		return // stopped, or chat revoked while queued; drop
	}
	err := m.sendOnce(api, msg)
	if err == nil {
		return
	}
	if ae, ok := asAPIError(err); ok && ae.Code == 429 && ae.RetryAfter > 0 {
		// Telegram is throttling the bot: honour it (capped), then try
		// once more with a FRESH timeout (retry_after routinely exceeds
		// the first attempt's budget). Sleeping here only delays the
		// send worker - the tick path stays untouched (full outbox just
		// drops).
		wait := ae.RetryAfter
		if wait > time.Minute {
			wait = time.Minute
		}
		time.Sleep(wait)
		if err = m.sendOnce(api, msg); err == nil {
			return
		}
	}
	m.setErr(err.Error())
	m.log.Debug("telegram send failed; dropping message", "chat", msg.chatID, "err", err)
}

// sendOnce is one sendMessage attempt on its own timeout budget.
func (m *Manager) sendOnce(api *apiClient, msg outMsg) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return api.sendMessage(ctx, msg.chatID, msg.text)
}

func (m *Manager) setErr(s string) {
	m.stateMu.Lock()
	m.lastErr = s
	m.stateMu.Unlock()
}

// sleepCtx sleeps d or until ctx cancels; false means cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Compile-time check: the Manager is the driver's Conn.
var _ Conn = (*Manager)(nil)
