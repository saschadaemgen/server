package rtppacer

import (
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// recWriter records the headers it is asked to write, in order.
type recWriter struct {
	mu   sync.Mutex
	seqs []uint16
	ssrc []uint32
}

func (w *recWriter) Write(h *rtp.Header, payload []byte, _ interceptor.Attributes) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seqs = append(w.seqs, h.SequenceNumber)
	w.ssrc = append(w.ssrc, h.SSRC)

	return h.MarshalSize() + len(payload), nil
}

func (w *recWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return len(w.seqs)
}

func (w *recWriter) snapshotSeqs() []uint16 {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]uint16(nil), w.seqs...)
}

func quietPacer(rate int) *Pacer {
	return newPacer(rate, time.Millisecond, log.New(io.Discard, "", 0))
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// TestPacer_PassThroughNoDropMixedSSRC is the core regression vs
// gcc.LeakyBucketPacer: packets on DIFFERENT SSRCs (media + a FEC-like + an
// RTX-like ssrc) must ALL reach the downstream, in order, none dropped.
func TestPacer_PassThroughNoDropMixedSSRC(t *testing.T) {
	p := quietPacer(100_000_000) // high rate -> drains promptly
	t.Cleanup(func() { _ = p.Close() })

	fw := &recWriter{}
	w := p.BindLocalStream(&interceptor.StreamInfo{SSRC: 1}, fw)

	const n = 30
	ssrcs := []uint32{1, 1, 2, 1, 3} // media, media, fec-like, media, rtx-like
	for i := 0; i < n; i++ {
		h := &rtp.Header{SSRC: ssrcs[i%len(ssrcs)], SequenceNumber: uint16(i)}
		if _, err := w.Write(h, []byte{0xAA, 0xBB, 0xCC}, nil); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	waitFor(t, func() bool { return fw.count() == n }, "all packets paced through")

	// Order preserved (FIFO).
	got := fw.snapshotSeqs()
	for i, s := range got {
		if s != uint16(i) {
			t.Fatalf("out of order at %d: got seq %d", i, s)
		}
	}
}

// TestPacer_Close_Flushes: packets queued at close are flushed, not lost.
func TestPacer_Close_Flushes(t *testing.T) {
	// Low rate + tiny burst so packets actually queue rather than drain on the
	// first tick, proving flush() releases the backlog.
	p := newPacer(8000, time.Millisecond, log.New(io.Discard, "", 0)) // 1000 B/s
	fw := &recWriter{}
	w := p.BindLocalStream(&interceptor.StreamInfo{SSRC: 1}, fw)

	const n = 10
	for i := 0; i < n; i++ {
		if _, err := w.Write(&rtp.Header{SSRC: 1, SequenceNumber: uint16(i)}, make([]byte, 1200), nil); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	_ = p.Close() // flush() must drain the remaining backlog

	waitFor(t, func() bool { return fw.count() == n }, "backlog flushed on close")
}

// TestPacer_WriteAfterClose_PassesThrough: a late write during teardown is sent
// straight through, never panics.
func TestPacer_WriteAfterClose_PassesThrough(t *testing.T) {
	p := quietPacer(100_000_000)
	fw := &recWriter{}
	w := p.BindLocalStream(&interceptor.StreamInfo{SSRC: 1}, fw)
	_ = p.Close()

	if _, err := w.Write(&rtp.Header{SSRC: 1, SequenceNumber: 1}, []byte{1}, nil); err != nil {
		t.Fatalf("write after close: %v", err)
	}
	if fw.count() != 1 {
		t.Errorf("write after close not passed through: count=%d", fw.count())
	}
}

func TestPacer_CloseIdempotent(t *testing.T) {
	p := quietPacer(100_000_000)
	if err := p.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close 2 (idempotent): %v", err)
	}
}

func TestFactory_NewInterceptorDistinct(t *testing.T) {
	f := NewFactory(2_400_000, log.New(io.Discard, "", 0))
	a, err := f.NewInterceptor("a")
	if err != nil {
		t.Fatalf("new a: %v", err)
	}
	b, err := f.NewInterceptor("b")
	if err != nil {
		t.Fatalf("new b: %v", err)
	}
	if a == nil || b == nil {
		t.Fatal("nil interceptor")
	}
	if a == b {
		t.Error("factory must return distinct per-PC instances")
	}
	_ = a.Close()
	_ = b.Close()
}
