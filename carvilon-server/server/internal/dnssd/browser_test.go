package dnssd

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// TestBrowserLifecycle: Open never returns nil, Scan never panics in any
// mode, and Close is clean + idempotent.
func TestBrowserLifecycle(t *testing.T) {
	b := Open(shellyService, nil)
	if b == nil {
		t.Fatal("Open returned nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	b.Scan(ctx) // must not panic in whatever mode we got
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
}

// TestBrowserLoopbackReceive is a best-effort real-socket check: when the
// browser came up passive, an announcement multicast on the LAN group is
// received and surfaced. It SKIPs (not fails) where the OS/network cannot
// deliver multicast to ourselves - the deterministic coverage is in the
// parser + coordinator tests; this only exercises the wire on hosts that
// allow it (the RPi path).
func TestBrowserLoopbackReceive(t *testing.T) {
	b := Open(shellyService, nil)
	defer b.Close()
	if b.Mode() != ModePassive {
		t.Skipf("browser mode %s; multicast receive not exercisable here", b.Mode())
	}

	pkt := buildAnnouncement(t,
		"shellytest-aabbccddeeff._shelly._tcp.local.",
		"shellytest-aabbccddeeff.local.",
		netip.MustParseAddr("192.168.222.222"), 80,
		[]string{"gen=2", "id=shellytest-aabbccddeeff"})

	// Announce from an ephemeral socket to the group.
	sender, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		t.Skipf("no sender socket: %v", err)
	}
	defer sender.Close()

	deadline := time.After(3 * time.Second)
	done := make(chan Entry, 1)
	go func() {
		for {
			select {
			case e := <-b.Entries():
				if e.InstanceLabel() == "shellytest-aabbccddeeff" {
					done <- e
					return
				}
			case <-deadline:
				return
			}
		}
	}()

	// Send a few times - multicast is lossy.
	for i := 0; i < 5; i++ {
		_, _ = sender.WriteTo(pkt, mdnsGroup)
		time.Sleep(120 * time.Millisecond)
	}

	select {
	case e := <-done:
		if len(e.Addrs) != 1 || e.Addrs[0] != netip.MustParseAddr("192.168.222.222") {
			t.Fatalf("received entry addrs = %v", e.Addrs)
		}
	case <-deadline:
		t.Skip("no multicast loopback delivery in this environment")
	}
}
