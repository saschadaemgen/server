package stream

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestTURNClientSet_AddRemoveSnapshot(t *testing.T) {
	s := newTURNClientSet()
	if got := len(s.snapshot()); got != 0 {
		t.Fatalf("empty set snapshot len = %d, want 0", got)
	}
	s.add(TURNClient{SrcAddr: "203.0.113.5:1", Username: "u1"})
	s.add(TURNClient{SrcAddr: "203.0.113.6:2", Username: "u2"})
	if got := len(s.snapshot()); got != 2 {
		t.Fatalf("after 2 adds len = %d, want 2", got)
	}
	// Re-adding the same SrcAddr overwrites, never duplicates (one
	// allocation per five-tuple).
	s.add(TURNClient{SrcAddr: "203.0.113.5:1", Username: "u1-renewed"})
	if got := len(s.snapshot()); got != 2 {
		t.Fatalf("after re-add of same key len = %d, want 2", got)
	}
	s.remove("203.0.113.5:1")
	snap := s.snapshot()
	if len(snap) != 1 || snap[0].SrcAddr != "203.0.113.6:2" {
		t.Fatalf("after remove, snapshot = %+v", snap)
	}
	// Removing an unknown key is a no-op.
	s.remove("not-present:0")
	if got := len(s.snapshot()); got != 1 {
		t.Fatalf("remove unknown changed len to %d", got)
	}
}

// TestTURNClientSet_Concurrent hammers the set from many goroutines while
// pollers snapshot concurrently - the race detector (-race) must stay clean
// (pion fires the lifecycle callbacks while the master polls TURNStats).
func TestTURNClientSet_Concurrent(t *testing.T) {
	s := newTURNClientSet()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "203.0.113.5:" + strconv.Itoa(i)
			s.add(TURNClient{SrcAddr: key, Username: "u" + strconv.Itoa(i)})
			_ = s.snapshot()
			s.remove(key)
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 64; j++ {
				_ = s.snapshot()
			}
		}()
	}
	wg.Wait()
}

// TestTURNEventHandler_AllocationLifecycle drives the pion EventHandler
// callbacks directly (no socket) and asserts the open-core TURNEvent carries
// the raw + masked address, the relay address as DstAddr, and that the live
// client set is maintained created->present, deleted->gone.
func TestTURNEventHandler_AllocationLifecycle(t *testing.T) {
	set := newTURNClientSet()
	var mu sync.Mutex
	var events []TURNEvent
	h := newTURNEventHandler(set, func(e TURNEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	src := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 54321}
	srv := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 3478}
	relay := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 49000}

	h.OnAllocationCreated(src, srv, "udp", "1700000000:carvilon", "carvilon", relay, 0)

	if got := len(set.snapshot()); got != 1 {
		t.Fatalf("after created, client set len = %d, want 1", got)
	}
	mu.Lock()
	created := events[len(events)-1]
	mu.Unlock()

	if created.Kind != "allocation_created" {
		t.Errorf("Kind = %q, want allocation_created", created.Kind)
	}
	if created.SrcAddr != "203.0.113.5:54321" {
		t.Errorf("SrcAddr = %q, want the raw client address", created.SrcAddr)
	}
	if strings.Contains(created.SrcAddrMasked, "203.0.113.5") {
		t.Errorf("SrcAddrMasked %q leaks the full IP", created.SrcAddrMasked)
	}
	if !strings.Contains(created.SrcAddrMasked, "x.x") {
		t.Errorf("SrcAddrMasked %q lacks the mask marker", created.SrcAddrMasked)
	}
	if created.DstAddr != "198.51.100.1:49000" {
		t.Errorf("DstAddr = %q, want the relay address", created.DstAddr)
	}
	if created.DstAddrMasked == "" || strings.Contains(created.DstAddrMasked, "100.1") {
		t.Errorf("DstAddrMasked %q is not masked", created.DstAddrMasked)
	}
	if created.Protocol != "udp" || created.Username != "1700000000:carvilon" || created.Realm != "carvilon" {
		t.Errorf("created event fields wrong: %+v", created)
	}
	if created.AuthOK != nil {
		t.Errorf("AuthOK must be nil on a non-auth event, got %v", *created.AuthOK)
	}
	if created.Time.IsZero() {
		t.Errorf("created event has a zero timestamp")
	}

	h.OnAllocationDeleted(src, srv, "udp", "1700000000:carvilon", "carvilon")
	if got := len(set.snapshot()); got != 0 {
		t.Fatalf("after deleted, client set len = %d, want 0", got)
	}
	mu.Lock()
	deleted := events[len(events)-1]
	mu.Unlock()
	if deleted.Kind != "allocation_deleted" || deleted.SrcAddr != "203.0.113.5:54321" {
		t.Errorf("deleted event wrong: %+v", deleted)
	}
	// A delete carries no relay address.
	if deleted.DstAddr != "" {
		t.Errorf("deleted event DstAddr = %q, want empty", deleted.DstAddr)
	}
}

// TestTURNEventHandler_Auth checks the auth verdict surfaces as *bool and
// that an auth event does NOT register a client (only created does).
func TestTURNEventHandler_Auth(t *testing.T) {
	set := newTURNClientSet()
	var got *TURNEvent
	h := newTURNEventHandler(set, func(e TURNEvent) { ev := e; got = &ev })

	src := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 1}
	h.OnAuth(src, src, "tcp", "u", "carvilon", "Allocate", true)

	if got == nil || got.Kind != "auth" {
		t.Fatalf("auth event not emitted: %+v", got)
	}
	if got.AuthOK == nil || *got.AuthOK != true {
		t.Errorf("AuthOK = %v, want non-nil true", got.AuthOK)
	}
	if got.Protocol != "tcp" {
		t.Errorf("Protocol = %q, want tcp", got.Protocol)
	}
	if len(set.snapshot()) != 0 {
		t.Errorf("OnAuth must not add to the client set")
	}

	// A failed verdict surfaces as *false (not nil).
	h.OnAuth(src, src, "udp", "u", "carvilon", "Allocate", false)
	if got.AuthOK == nil || *got.AuthOK != false {
		t.Errorf("failed-auth AuthOK = %v, want non-nil false", got.AuthOK)
	}
}

// TestTURNEventHandler_Error checks the readloop error surfaces in Err with
// no username (pion supplies none) and adds no client.
func TestTURNEventHandler_Error(t *testing.T) {
	set := newTURNClientSet()
	var got *TURNEvent
	h := newTURNEventHandler(set, func(e TURNEvent) { ev := e; got = &ev })
	src := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 1}
	h.OnAllocationError(src, src, "udp", "read: connection reset by peer")
	if got == nil || got.Kind != "allocation_error" {
		t.Fatalf("error event not emitted: %+v", got)
	}
	if got.Err != "read: connection reset by peer" {
		t.Errorf("Err = %q", got.Err)
	}
	if got.AuthOK != nil {
		t.Errorf("AuthOK must be nil on an error event")
	}
}

// TestTURNEventHandler_NoSecretLeak asserts the TURN shared secret never
// surfaces in any event field. The ephemeral REST username IS public (it
// travels in the SDP); the SECRET is what must stay invisible. pion never
// hands the secret to the EventHandler - this asserts that contract holds
// across every callback.
func TestTURNEventHandler_NoSecretLeak(t *testing.T) {
	const secret = "SUPER-SECRET-shared-key" // sentinel; must never appear

	set := newTURNClientSet()
	var fields []string
	h := newTURNEventHandler(set, func(e TURNEvent) {
		fields = append(fields, e.Kind, e.SrcAddr, e.SrcAddrMasked,
			e.DstAddr, e.DstAddrMasked, e.Protocol, e.Username, e.Realm, e.Err)
	})

	src := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 1}
	relay := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 2}
	h.OnAllocationCreated(src, src, "udp", "1700000000:carvilon", "carvilon", relay, 0)
	h.OnAuth(src, src, "udp", "1700000000:carvilon", "carvilon", "Allocate", false)
	h.OnAllocationError(src, src, "udp", "read: connection reset")
	h.OnAllocationDeleted(src, src, "udp", "1700000000:carvilon", "carvilon")

	for _, f := range fields {
		if strings.Contains(f, secret) {
			t.Fatalf("an event field leaked the shared secret: %q", f)
		}
	}
}

// TestTURNEventHandler_NilCallback proves a nil OnTURNEvent is safe (the set
// is still maintained, no panic).
func TestTURNEventHandler_NilCallback(t *testing.T) {
	set := newTURNClientSet()
	h := newTURNEventHandler(set, nil)
	src := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 1}
	relay := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 2}
	h.OnAllocationCreated(src, src, "udp", "u", "carvilon", relay, 0)
	if got := len(set.snapshot()); got != 1 {
		t.Fatalf("nil callback: client set len = %d, want 1", got)
	}
	h.OnAllocationDeleted(src, src, "udp", "u", "carvilon")
	if got := len(set.snapshot()); got != 0 {
		t.Fatalf("nil callback: client set len = %d, want 0", got)
	}
}

// TestCloudServer_TURNStatsDisabled verifies the disabled-relay snapshot
// (no public IP) reports Enabled:false with zero values.
func TestCloudServer_TURNStatsDisabled(t *testing.T) {
	cs := &cloudServer{} // turnSrv/clients nil = TURN soft-gated off
	stats := cs.TURNStats()
	if stats.Enabled {
		t.Errorf("Enabled = true, want false when TURN is off")
	}
	if stats.AllocationCount != 0 || len(stats.Clients) != 0 {
		t.Errorf("disabled stats not zero: %+v", stats)
	}
}
