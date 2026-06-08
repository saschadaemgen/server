package streamhub

import (
	"context"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// newTestTrack builds a standalone fan-out track (no PeerConnection
// needed) for the SetTrack/WaitTrack tests.
func newTestTrack(t *testing.T, id string) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	tr, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", id)
	if err != nil {
		t.Fatalf("new track: %v", err)
	}
	return tr
}

func TestHub_AddThenGet(t *testing.T) {
	h := NewHub()
	s := &Session{StreamID: "cam-1"}
	if err := h.Add(s); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok := h.Get("cam-1")
	if !ok {
		t.Fatal("Get returned ok=false after Add")
	}
	if got != s {
		t.Errorf("Get returned a different session pointer")
	}
}

func TestHub_DuplicateAddConflicts(t *testing.T) {
	h := NewHub()
	if err := h.Add(&Session{StreamID: "cam-1"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	err := h.Add(&Session{StreamID: "cam-1"})
	if err != ErrConflict {
		t.Errorf("second Add error = %v, want ErrConflict", err)
	}
	// A different streamID must still be accepted.
	if err := h.Add(&Session{StreamID: "cam-2"}); err != nil {
		t.Errorf("Add of distinct streamID: %v", err)
	}
}

func TestHub_RemoveThenGetNotFound(t *testing.T) {
	h := NewHub()
	_ = h.Add(&Session{StreamID: "cam-1"})
	h.Remove("cam-1")
	if _, ok := h.Get("cam-1"); ok {
		t.Error("Get returned ok=true after Remove")
	}
	// Re-publishing the same streamID after Remove must succeed (no
	// lingering conflict).
	if err := h.Add(&Session{StreamID: "cam-1"}); err != nil {
		t.Errorf("re-Add after Remove: %v", err)
	}
}

func TestHub_RemoveWithoutAddIsNoop(t *testing.T) {
	h := NewHub()
	// Must not panic, must not call any OnClose (there is none).
	h.Remove("never-added")
}

func TestHub_OnCloseFiresExactlyOnce(t *testing.T) {
	h := NewHub()
	calls := 0
	_ = h.Add(&Session{StreamID: "cam-1", OnClose: func() { calls++ }})

	h.Remove("cam-1")
	h.Remove("cam-1") // idempotent: must not fire OnClose again

	if calls != 1 {
		t.Errorf("OnClose called %d times, want exactly 1", calls)
	}
}

func TestHub_RemoveNilOnCloseIsSafe(t *testing.T) {
	h := NewHub()
	_ = h.Add(&Session{StreamID: "cam-1"}) // OnClose nil
	h.Remove("cam-1")                      // must not panic on nil OnClose
}

// --- S2-05: ready/SetTrack/WaitTrack race resolution ------------------------

func TestSession_SetTrackThenWaitReturnsImmediately(t *testing.T) {
	sess := NewSession("cam-1", nil, nil)
	track := newTestTrack(t, "cam-1")
	sess.SetTrack(track)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := sess.WaitTrack(ctx)
	if err != nil {
		t.Fatalf("WaitTrack after SetTrack: %v", err)
	}
	if got != track {
		t.Errorf("WaitTrack returned a different track pointer")
	}
}

func TestSession_WaitTrackExpiredCtx(t *testing.T) {
	sess := NewSession("cam-1", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done

	got, err := sess.WaitTrack(ctx)
	if err == nil {
		t.Fatal("WaitTrack with expired ctx returned nil error")
	}
	if got != nil {
		t.Errorf("WaitTrack returned non-nil track on ctx error")
	}
}

func TestSession_WaitTrackBlocksUntilSetTrack(t *testing.T) {
	sess := NewSession("cam-1", nil, nil)
	track := newTestTrack(t, "cam-1")

	// SetTrack fires from another goroutine after a short delay; WaitTrack
	// must block until then, not return nil early.
	go func() {
		time.Sleep(50 * time.Millisecond)
		sess.SetTrack(track)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := sess.WaitTrack(ctx)
	if err != nil {
		t.Fatalf("WaitTrack blocked past the parallel SetTrack: %v", err)
	}
	if got != track {
		t.Errorf("WaitTrack returned a different track pointer")
	}
}

func TestSession_SetTrackOnlyFirstWins(t *testing.T) {
	sess := NewSession("cam-1", nil, nil)
	first := newTestTrack(t, "first")
	second := newTestTrack(t, "second")

	sess.SetTrack(first)
	sess.SetTrack(second) // must be a no-op (trackOnce)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := sess.WaitTrack(ctx)
	if err != nil {
		t.Fatalf("WaitTrack: %v", err)
	}
	if got != first {
		t.Errorf("WaitTrack returned the second track; first must win (trackOnce)")
	}
}
