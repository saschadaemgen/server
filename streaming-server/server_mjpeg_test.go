package stream

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"carvilon.local/stream/internal/stats"
)

// deadlineWriter simulates a TCP socket under a write deadline: each Write
// blocks until the most recently armed deadline elapses, then fails with
// os.ErrDeadlineExceeded — exactly how a wedged consumer behaves once the
// kernel send buffer is full and the per-frame deadline (the S20 backstop)
// has been set. With no deadline armed it would block indefinitely, which is
// precisely the pre-S20 hang the backstop exists to break.
type deadlineWriter struct {
	mu       sync.Mutex
	deadline time.Time
	armed    bool
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	d.deadline, d.armed = t, true
	d.mu.Unlock()
	return nil
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	d.mu.Lock()
	dl, armed := d.deadline, d.armed
	d.mu.Unlock()
	if !armed {
		// No backstop -> the pathological forever-block. Tests always arm a
		// deadline, so a bounded sleep just guards against a hung test run.
		time.Sleep(5 * time.Second)
		return 0, errors.New("deadlineWriter: no deadline armed")
	}
	if wait := time.Until(dl); wait > 0 {
		time.Sleep(wait)
	}
	return 0, os.ErrDeadlineExceeded
}

// TestStreamMJPEG_WriteStallTearsDown is the S20 autark proof for the ESP/MJPEG
// path: a consumer that stops reading WITHOUT closing the connection (no FIN,
// so the request context never cancels and the channel never closes) used to
// pin the profile "active" forever — writer.Write blocked on the full send
// buffer and the deferred Unregister never ran. The per-frame write-deadline
// backstop turns that stalled write into an error within mjpegWriteTimeout, so
// streamMJPEG returns and the row clears.
func TestStreamMJPEG_WriteStallTearsDown(t *testing.T) {
	prev := mjpegWriteTimeout
	mjpegWriteTimeout = 20 * time.Millisecond
	defer func() { mjpegWriteTimeout = prev }()

	reg := stats.New()
	sc := reg.Register("mjpeg_bal", "mjpeg", "192.168.1.28:5000")

	frames := make(chan []byte, 1)
	frames <- []byte("\xff\xd8jpeg\xff\xd9") // a frame the wedged consumer never drains

	dw := &deadlineWriter{}
	done := make(chan struct{})
	go func() {
		// Mirror handleMJPEG: streamMJPEG runs, then the deferred Unregister.
		streamMJPEG(context.Background(), dw, func() error { return nil }, dw.SetWriteDeadline, frames, sc)
		reg.Unregister(sc)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamMJPEG did not return on a stalled write; the wedged-consumer backstop is broken")
	}
	if reg.Count() != 0 {
		t.Fatalf("stats client still registered after teardown: Count = %d, want 0 (row must read idle)", reg.Count())
	}
}

// TestStreamMJPEG_ContextCancelTearsDown pins the CLEAN-disconnect path (the
// one that already worked): when the consumer closes its connection the request
// context cancels, streamMJPEG returns immediately, and the deferred Unregister
// clears the row. setDeadline is nil here, exercising the nil-safe backstop
// (no deadline support -> pre-S20 behaviour, still correct).
func TestStreamMJPEG_ContextCancelTearsDown(t *testing.T) {
	reg := stats.New()
	sc := reg.Register("mjpeg_bal", "mjpeg", "192.168.1.28:5000")

	frames := make(chan []byte) // no frames; the consumer aborts cleanly
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		streamMJPEG(ctx, &deadlineWriter{}, func() error { return nil }, nil, frames, sc)
		reg.Unregister(sc)
		close(done)
	}()

	cancel() // client closed the connection -> request context cancelled
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamMJPEG did not return on context cancel")
	}
	if reg.Count() != 0 {
		t.Fatalf("stats client still registered after clean abort: Count = %d, want 0", reg.Count())
	}
}

// TestStreamMJPEG_DeliversFramesThenChannelClose guards the happy path: frames
// are written and counted, each one flushed, and a closed frame channel (encoder
// died / shutdown) ends the loop cleanly. It also pins that the backstop is
// ACTIVE yet harmless on a healthy stream - the write deadline is armed once per
// frame, but a consumer that keeps up is never torn down (the loop ends only on
// the channel close). Proves the backstop refactor did not change the normal
// streaming contract.
func TestStreamMJPEG_DeliversFramesThenChannelClose(t *testing.T) {
	reg := stats.New()
	sc := reg.Register("mjpeg_bal", "mjpeg", "192.168.1.28:5000")

	frames := make(chan []byte, 2)
	frames <- []byte("aaa")
	frames <- []byte("bbbb")
	close(frames) // session ends after the buffered frames drain

	var flushes, deadlines int
	streamMJPEG(
		context.Background(),
		io.Discard,
		func() error { flushes++; return nil },
		func(time.Time) error { deadlines++; return nil },
		frames, sc,
	)

	if got := reg.Snapshot().Profiles["mjpeg_bal"].FramesSent; got != 2 {
		t.Fatalf("FramesSent = %d, want 2", got)
	}
	if flushes != 2 {
		t.Fatalf("flushes = %d, want 2 (one per delivered frame)", flushes)
	}
	if deadlines != 2 {
		t.Fatalf("write deadline armed %d times, want 2 (the backstop must be active per frame)", deadlines)
	}
}
