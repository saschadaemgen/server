package console

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fakes ----

// fakeTransport is an in-memory Transport: the test feeds client messages
// on `in`, reads backend output on `out`, and calls clientClose to
// simulate the browser going away.
type fakeTransport struct {
	in     chan ClientMsg
	out    chan []byte
	status chan []byte
	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		in:     make(chan ClientMsg, 16),
		out:    make(chan []byte, 256),
		status: make(chan []byte, 16),
		done:   make(chan struct{}),
	}
}

func (f *fakeTransport) Recv(ctx context.Context) (ClientMsg, error) {
	select {
	case <-ctx.Done():
		return ClientMsg{}, ctx.Err()
	case <-f.done:
		return ClientMsg{}, io.EOF
	case m := <-f.in:
		return m, nil
	}
}

func (f *fakeTransport) SendOutput(ctx context.Context, p []byte) error {
	b := append([]byte(nil), p...)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case f.out <- b:
		return nil
	}
}

func (f *fakeTransport) SendStatus(ctx context.Context, line []byte) error {
	b := append([]byte(nil), line...)
	select {
	case f.status <- b:
	default:
	}
	return nil
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
	}
	return nil
}

// fakeBackend is a controllable Backend: the test pushes output on
// readCh, observes client input on written, and observes resizes.
type fakeBackend struct {
	readCh     chan []byte
	written    chan []byte
	resizes    chan [2]uint16
	closeOnce  sync.Once
	closed     chan struct{}
	closedFlag atomic.Bool
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		readCh:  make(chan []byte, 16),
		written: make(chan []byte, 16),
		resizes: make(chan [2]uint16, 16),
		closed:  make(chan struct{}),
	}
}

func (b *fakeBackend) Read(p []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, io.EOF
	case chunk := <-b.readCh:
		return copy(p, chunk), nil
	}
}

func (b *fakeBackend) Write(p []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, io.ErrClosedPipe
	case b.written <- append([]byte(nil), p...):
		return len(p), nil
	}
}

func (b *fakeBackend) Resize(cols, rows uint16) error {
	select {
	case b.resizes <- [2]uint16{cols, rows}:
	default:
	}
	return nil
}

func (b *fakeBackend) Close() error {
	b.closeOnce.Do(func() {
		b.closedFlag.Store(true)
		close(b.closed)
	})
	return nil
}

// ---- tests ----

func TestManager_ConcurrencyCap(t *testing.T) {
	m := NewManager(nil, WithMaxSessions(4))
	backends := make([]*fakeBackend, 4)
	for i := range backends {
		backends[i] = newFakeBackend()
		if _, err := m.Open("ssh", "t", "admin", backends[i]); err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
	}
	extra := newFakeBackend()
	if _, err := m.Open("ssh", "t", "admin", extra); err != ErrTooManySessions {
		t.Fatalf("5th Open err = %v, want ErrTooManySessions", err)
	}
	if m.Count() != 4 {
		t.Fatalf("Count = %d, want 4", m.Count())
	}
}

func TestManager_FourIndependentSessions(t *testing.T) {
	m := NewManager(nil, WithMaxSessions(4), WithIdleTimeout(0))
	type rig struct {
		b *fakeBackend
		t *fakeTransport
		s *Session
	}
	rigs := make([]*rig, 4)
	var wg sync.WaitGroup
	for i := range rigs {
		b, tr := newFakeBackend(), newFakeTransport()
		s, err := m.Open("ssh", "term", "admin", b)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		rigs[i] = &rig{b: b, t: tr, s: s}
		wg.Add(1)
		go func(r *rig) { defer wg.Done(); m.Serve(context.Background(), r.s, r.t) }(rigs[i])
	}
	if m.Count() != 4 {
		t.Fatalf("Count = %d, want 4", m.Count())
	}
	// Each backend's output must reach only its own transport.
	for i, r := range rigs {
		want := []byte{byte('A' + i)}
		r.b.readCh <- want
		select {
		case got := <-r.t.out:
			if len(got) != 1 || got[0] != want[0] {
				t.Fatalf("session %d got %q, want %q", i, got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("session %d: no output", i)
		}
	}
	// Closing every transport (client disconnect) tears all sessions down.
	for _, r := range rigs {
		r.t.Close()
	}
	waitOrFatal(t, &wg, 3*time.Second)
	if m.Count() != 0 {
		t.Fatalf("after close Count = %d, want 0", m.Count())
	}
	for i, r := range rigs {
		if !r.b.closedFlag.Load() {
			t.Fatalf("session %d backend not closed", i)
		}
	}
}

func TestManager_TeardownClosesBackendNoLeak(t *testing.T) {
	m := NewManager(nil, WithIdleTimeout(0))
	b, tr := newFakeBackend(), newFakeTransport()
	s, err := m.Open("local", "shell", "admin", b)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	served := make(chan struct{})
	go func() { m.Serve(context.Background(), s, tr); close(served) }()

	// Client goes away.
	tr.Close()

	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after client close (goroutine leak)")
	}
	// Serve returns only after wg.Wait(): every spawned goroutine has
	// exited, the backend is closed, and the session is unregistered.
	if !b.closedFlag.Load() {
		t.Fatal("backend not closed on teardown")
	}
	if m.Count() != 0 {
		t.Fatalf("Count = %d after teardown, want 0", m.Count())
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("session Done not closed")
	}
}

func TestManager_BackendEOFEndsSession(t *testing.T) {
	m := NewManager(nil, WithIdleTimeout(0))
	b, tr := newFakeBackend(), newFakeTransport()
	s, _ := m.Open("ssh", "term", "admin", b)
	served := make(chan struct{})
	go func() { m.Serve(context.Background(), s, tr); close(served) }()

	// Remote side hangs up: Read returns EOF, which must end the session.
	b.Close()
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after backend EOF")
	}
	if m.Count() != 0 {
		t.Fatalf("Count = %d, want 0", m.Count())
	}
}

func TestManager_IdleTimeout(t *testing.T) {
	m := NewManager(nil, WithIdleTimeout(80*time.Millisecond))
	b, tr := newFakeBackend(), newFakeTransport()
	s, _ := m.Open("ssh", "term", "admin", b)
	served := make(chan struct{})
	go func() { m.Serve(context.Background(), s, tr); close(served) }()

	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("idle session was not torn down")
	}
	if !b.closedFlag.Load() {
		t.Fatal("idle teardown did not close the backend")
	}
}

func TestManager_ForwardsDataAndResize(t *testing.T) {
	m := NewManager(nil, WithIdleTimeout(0))
	b, tr := newFakeBackend(), newFakeTransport()
	s, _ := m.Open("ssh", "term", "admin", b)
	go m.Serve(context.Background(), s, tr)

	tr.in <- ClientMsg{Type: MsgData, Data: []byte("ls\n")}
	select {
	case got := <-b.written:
		if string(got) != "ls\n" {
			t.Fatalf("backend got %q, want %q", got, "ls\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("keystroke not forwarded to backend")
	}

	tr.in <- ClientMsg{Type: MsgResize, Cols: 132, Rows: 43}
	select {
	case got := <-b.resizes:
		if got != [2]uint16{132, 43} {
			t.Fatalf("resize = %v, want [132 43]", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resize not forwarded to backend")
	}
	tr.Close()
}

func TestManager_CloseAllEndsSessions(t *testing.T) {
	m := NewManager(nil, WithIdleTimeout(0))
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		b, tr := newFakeBackend(), newFakeTransport()
		s, _ := m.Open("ssh", "term", "admin", b)
		wg.Add(1)
		go func() { defer wg.Done(); m.Serve(context.Background(), s, tr) }()
	}
	m.CloseAll()
	waitOrFatal(t, &wg, 3*time.Second)
	if m.Count() != 0 {
		t.Fatalf("Count = %d after CloseAll, want 0", m.Count())
	}
}

func waitOrFatal(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("timed out waiting for sessions to end")
	}
}
