// Package console is the reusable session framework behind the terminal
// dock: the browser cannot open raw sockets or speak SSH, so the Go
// server is the bridge — pane <-> WebSocket <-> console.Session <->
// target. Step 1 wires the SSH terminal (a local shell PTY on the edge
// host, or an outbound SSH client) onto this frame; TCP and UDP consoles
// (step 2) become mere Backend adapters on the same frame.
//
// The Manager owns the live sessions: it caps concurrency, enforces an
// idle timeout, and guarantees clean teardown — Serve blocks until every
// goroutine it spawned has exited and the backend is closed, so a closed
// session leaks neither the process/SSH connection nor a reader/writer.
//
// The frame is transport-agnostic on purpose: Serve copies between a
// Backend (the target) and a Transport (the browser). Production plugs a
// WebSocket in as the Transport; tests plug an in-memory fake. That keeps
// the lifecycle unit-testable without a real socket.
package console

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Errors.
var (
	// ErrTooManySessions is returned by Open when the concurrency cap is
	// reached. The caller must close the backend it was about to register.
	ErrTooManySessions = errors.New("console: too many concurrent sessions")
	// ErrLocalShellUnsupported is returned by OpenLocalShell off the Linux
	// edge build. Defined here (platform-independent) so callers compile on
	// every target; only shell_other.go actually returns it.
	ErrLocalShellUnsupported = errors.New("console: local shell is only available on the Linux edge host")
)

// Defaults for the manager.
const (
	defaultMaxSessions = 16
	defaultIdleTimeout = 30 * time.Minute
	// readChunk bounds the per-read backend buffer: output is streamed in
	// at most this many bytes per WebSocket frame, capping memory and
	// giving natural backpressure through the Transport's blocking Send.
	readChunk = 32 * 1024
)

// MsgType classifies a decoded client->server message.
type MsgType int

const (
	// MsgData carries raw bytes for the backend (keystrokes / pasted text).
	MsgData MsgType = iota
	// MsgResize carries a new terminal window size.
	MsgResize
)

// ClientMsg is one decoded message from the browser.
type ClientMsg struct {
	Type MsgType
	Data []byte // MsgData
	Cols uint16 // MsgResize
	Rows uint16 // MsgResize
}

// Backend is the target end of a session: a local shell PTY, an outbound
// SSH session (step 1), or a TCP/UDP socket (step 2). Resize is a no-op
// for backends without a window concept.
type Backend interface {
	io.ReadWriteCloser
	// Resize adjusts the backend's terminal window (columns x rows).
	Resize(cols, rows uint16) error
}

// Transport is the browser side of a session — a WebSocket in production,
// an in-memory fake in tests. The bridge calls Recv from one goroutine
// and SendOutput/SendStatus from another; Close may be called once from
// either. Implementations must tolerate that split.
type Transport interface {
	// Recv blocks for the next client message; a non-nil error (io.EOF on
	// a clean client close) ends the session.
	Recv(ctx context.Context) (ClientMsg, error)
	// SendOutput delivers raw backend bytes to the client.
	SendOutput(ctx context.Context, p []byte) error
	// SendStatus delivers a status/error control line (already-marshalled
	// JSON) to the client out of band from the byte stream.
	SendStatus(ctx context.Context, line []byte) error
	// Close releases the transport.
	Close() error
}

// Info is a read view of one live session for listings / diagnostics.
type Info struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// Session is one live console: a registered Backend plus its lifecycle
// bookkeeping. Callers obtain one from Manager.Open and then run
// Manager.Serve to bridge it to a Transport.
type Session struct {
	ID        string
	Kind      string
	Title     string
	User      string
	CreatedAt time.Time

	mgr     *Manager
	backend Backend
	lastAct atomic.Int64 // unix-nano of the last byte in either direction
	done    chan struct{}
}

// Done is closed once the session has fully torn down.
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) touch() { s.lastAct.Store(time.Now().UnixNano()) }

// Manager owns the live console sessions.
type Manager struct {
	mu   sync.Mutex
	byID map[string]*Session
	seq  uint64

	log       *slog.Logger
	max       int
	idle      time.Duration
	readLimit int64
}

// Option configures a Manager.
type Option func(*Manager)

// WithMaxSessions caps concurrent sessions (default 16).
func WithMaxSessions(n int) Option {
	return func(m *Manager) {
		if n > 0 {
			m.max = n
		}
	}
}

// WithIdleTimeout ends a session after this much inactivity in both
// directions (default 30m; zero disables the watchdog).
func WithIdleTimeout(d time.Duration) Option {
	return func(m *Manager) { m.idle = d }
}

// NewManager constructs a Manager.
func NewManager(log *slog.Logger, opts ...Option) *Manager {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &Manager{
		byID: map[string]*Session{},
		log:  log.With("component", "console"),
		max:  defaultMaxSessions,
		idle: defaultIdleTimeout,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Open registers backend as a new session and returns it. On
// ErrTooManySessions the caller still owns (and must close) backend.
func (m *Manager) Open(kind, title, user string, backend Backend) (*Session, error) {
	m.mu.Lock()
	if len(m.byID) >= m.max {
		m.mu.Unlock()
		return nil, ErrTooManySessions
	}
	m.seq++
	id := kind + "-" + strconv.FormatUint(m.seq, 10)
	s := &Session{
		ID:        id,
		Kind:      kind,
		Title:     title,
		User:      user,
		CreatedAt: time.Now(),
		mgr:       m,
		backend:   backend,
		done:      make(chan struct{}),
	}
	s.touch()
	m.byID[id] = s
	m.mu.Unlock()
	m.log.Info("session opened", "id", id, "kind", kind, "user", user)
	return s, nil
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.byID, id)
	m.mu.Unlock()
}

// Count returns the number of live sessions.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID)
}

// List returns a snapshot of the live sessions.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.byID))
	for _, s := range m.byID {
		out = append(out, Info{
			ID: s.ID, Kind: s.Kind, Title: s.Title, User: s.User, CreatedAt: s.CreatedAt,
		})
	}
	return out
}

// CloseAll tears down every live session's backend. Serve loops observe
// the backend error and unwind on their own; used at server shutdown.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	backends := make([]Backend, 0, len(m.byID))
	for _, s := range m.byID {
		backends = append(backends, s.backend)
	}
	m.mu.Unlock()
	for _, b := range backends {
		_ = b.Close()
	}
}

// Serve bridges s's backend to t until either side closes, ctx is
// cancelled, or the idle timeout elapses, then tears everything down
// exactly once. It blocks until teardown is complete — so when Serve
// returns, no goroutine it started is still running and the backend is
// closed. The session is removed from the manager and s.Done is closed.
func (m *Manager) Serve(ctx context.Context, s *Session, t Transport) {
	ctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup

	// backend -> client: stream output in bounded chunks.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		buf := make([]byte, readChunk)
		for {
			n, rerr := s.backend.Read(buf)
			if n > 0 {
				s.touch()
				if werr := t.SendOutput(ctx, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// idle watchdog: cancel when both directions have been quiet too long.
	if m.idle > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := m.idle / 2
			if tick <= 0 {
				tick = m.idle
			}
			tk := time.NewTicker(tick)
			defer tk.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tk.C:
					last := time.Unix(0, s.lastAct.Load())
					if time.Since(last) >= m.idle {
						m.log.Info("session idle-timeout", "id", s.ID)
						cancel()
						return
					}
				}
			}
		}()
	}

	// client -> backend: this goroutine drives the read loop.
	m.pumpClient(ctx, s, t)
	cancel()

	// Teardown: close the backend and transport, then wait for the
	// output/watchdog goroutines to observe the cancellation and exit.
	_ = s.backend.Close()
	_ = t.Close()
	wg.Wait()
	m.remove(s.ID)
	close(s.done)
	m.log.Info("session closed", "id", s.ID)
}

// pumpClient copies decoded client messages into the backend until the
// transport reports an error (client gone) or ctx is cancelled.
func (m *Manager) pumpClient(ctx context.Context, s *Session, t Transport) {
	for {
		msg, err := t.Recv(ctx)
		if err != nil {
			return
		}
		s.touch()
		switch msg.Type {
		case MsgData:
			if _, err := s.backend.Write(msg.Data); err != nil {
				return
			}
		case MsgResize:
			_ = s.backend.Resize(msg.Cols, msg.Rows)
		}
	}
}
