package whip

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"testing"

	"carvilon.local/stream/internal/streamhub"
)

// TestConsumerCount_Methods unit-tests the leak-safety arithmetic of the
// per-streamID WHEP consumer counter directly (S20): a decrement never drops
// below zero, the key is removed at zero (so an idle stream reports no entry,
// not a sticky 0), streams are independent, and ConsumerCounts returns a copy
// that cannot mutate the server's map. This is the arithmetic half of the
// briefing's HAUPTRISIKO; the wiring half (dec fires exactly once per teardown)
// is covered by the WHEP round-trip below plus the live 4G verification.
func TestConsumerCount_Methods(t *testing.T) {
	srv, err := New(Config{
		CertFile: "unused-no-listen",
		KeyFile:  "unused-no-listen",
		HMACKey:  bytes.Repeat([]byte{0xAB}, 32),
		Hub:      streamhub.NewHub(),
		Logger:   log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const a, b = "mac-a", "mac-b"

	if got := srv.ConsumerCounts(); len(got) != 0 {
		t.Fatalf("empty counts = %v, want {}", got)
	}

	// Two on a, one on b.
	srv.incConsumer(a)
	srv.incConsumer(a)
	srv.incConsumer(b)
	if got := srv.ConsumerCounts(); got[a] != 2 || got[b] != 1 {
		t.Fatalf("after inc: counts = %v, want a=2 b=1", got)
	}

	// Copy semantics: mutating the returned map must not affect the server.
	snap := srv.ConsumerCounts()
	snap[a] = 999
	if got := srv.ConsumerCounts(); got[a] != 2 {
		t.Fatalf("ConsumerCounts handed out a live map; a=%d after external mutation, want 2", got[a])
	}

	// Decrement a to zero -> key dropped (not a sticky 0).
	srv.decConsumer(a)
	srv.decConsumer(a)
	if _, ok := srv.ConsumerCounts()[a]; ok {
		t.Fatalf("zeroed stream %q still present; want key dropped", a)
	}

	// Over-decrement never goes negative (cannot leak the count downward).
	srv.decConsumer(a)
	srv.decConsumer(a)
	if got := srv.ConsumerCounts()[a]; got != 0 {
		t.Fatalf("over-decrement: a=%d, want 0 (floored)", got)
	}

	// b was untouched throughout a's churn.
	if got := srv.ConsumerCounts(); got[b] != 1 {
		t.Fatalf("b disturbed by a's churn: b=%d, want 1", got[b])
	}
}

// TestConsumerCount_RequestStopOnLastLeave is the S20 trigger half of the
// cloud-row-never-clears fix: the RequestStop callback fires EXACTLY on the
// genuine last-subscriber-leaves (1->0) transition, never while a viewer
// remains, and never on an over-decrement floor hit. decConsumer calls it
// synchronously (the goroutine decoupling lives in the cloud wiring), so the
// assertions need no synchronisation.
func TestConsumerCount_RequestStopOnLastLeave(t *testing.T) {
	var stops []string
	srv, err := New(Config{
		CertFile:    "unused-no-listen",
		KeyFile:     "unused-no-listen",
		HMACKey:     bytes.Repeat([]byte{0xAB}, 32),
		Hub:         streamhub.NewHub(),
		Logger:      log.New(io.Discard, "", 0),
		RequestStop: func(streamID string) { stops = append(stops, streamID) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const a, b = "mac-a", "mac-b"

	// Two subscribers on a, one leaves -> one still attached, no stop yet.
	srv.incConsumer(a)
	srv.incConsumer(a)
	srv.decConsumer(a)
	if len(stops) != 0 {
		t.Fatalf("stop fired while %q still has a subscriber: %v", a, stops)
	}

	// The last subscriber on a leaves -> exactly one stop for a.
	srv.decConsumer(a)
	if len(stops) != 1 || stops[0] != a {
		t.Fatalf("stops after last leave = %v, want [%s]", stops, a)
	}

	// Over-decrement (count already floored at 0) must NOT fire another stop.
	srv.decConsumer(a)
	if len(stops) != 1 {
		t.Fatalf("over-decrement fired a spurious stop: %v", stops)
	}

	// A different stream is independent: its own last-leave fires its own stop.
	srv.incConsumer(b)
	srv.decConsumer(b)
	if len(stops) != 2 || stops[1] != b {
		t.Fatalf("stops = %v, want second entry %s", stops, b)
	}
}

// TestConsumerCount_NoRequestStopCallbackSafe guards the nil-callback path:
// without a RequestStop wired (the pre-S20 / public build), a last-subscriber
// teardown is a plain no-op, never a panic.
func TestConsumerCount_NoRequestStopCallbackSafe(t *testing.T) {
	srv, err := New(Config{
		CertFile: "unused-no-listen",
		KeyFile:  "unused-no-listen",
		HMACKey:  bytes.Repeat([]byte{0xAB}, 32),
		Hub:      streamhub.NewHub(),
		Logger:   log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.incConsumer("mac-a")
	srv.decConsumer("mac-a") // must not panic with a nil RequestStop
}

// TestWHEP_ConsumerCountIncrementsOnAttach drives real WHEP subscribes and
// asserts the server's consumer count climbs per attached subscriber (S20).
// The increment runs synchronously at the attach boundary inside
// AcceptSubscriber (arm), BEFORE the 201 is written, so the count is exact the
// moment whepSubscribe returns - no teardown-timing dependence. The matching
// decrement on PeerConnection teardown is verified live over 4G (the
// briefing's designated Step-1 verification) and exercised under -race by the
// other WHEP tests' subscriber teardown.
func TestWHEP_ConsumerCountIncrementsOnAttach(t *testing.T) {
	base, key, srv := newTestServerH(t)
	const sid = "test-mac"
	startConnectedPublisher(t, base, sid, key)

	if got := srv.ConsumerCounts()[sid]; got != 0 {
		t.Fatalf("before subscribe: count[%s] = %d, want 0", sid, got)
	}

	r1 := whepSubscribe(t, base, sid)
	if r1.status != http.StatusCreated {
		t.Fatalf("subscribe 1 status = %d, want 201; body=%s", r1.status, r1.answerSDP)
	}
	if got := srv.ConsumerCounts()[sid]; got != 1 {
		t.Fatalf("after subscribe 1: count[%s] = %d, want 1", sid, got)
	}

	// Fan-out: a second subscriber on the same stream accumulates.
	r2 := whepSubscribe(t, base, sid)
	if r2.status != http.StatusCreated {
		t.Fatalf("subscribe 2 status = %d, want 201; body=%s", r2.status, r2.answerSDP)
	}
	if got := srv.ConsumerCounts()[sid]; got != 2 {
		t.Fatalf("after subscribe 2: count[%s] = %d, want 2 (fan-out accumulates)", sid, got)
	}
}
