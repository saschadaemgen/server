package mqttbroker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestMonitoredTopicGen1Trees pins the monitor's topic gate: carvilon/
// (Gen2+/first-party) and shellies/ (the fixed root Gen1 firmware
// prepends) are watched device trees, everything else - including the
// broker's own $SYS counters - stays off the device-monitoring page.
func TestMonitoredTopicGen1Trees(t *testing.T) {
	cases := []struct {
		topic string
		want  bool
	}{
		{"carvilon/x", true},
		{"shellies/x", true},
		{"shellies/shelly1-abc123/relay/0", true},
		{"$SYS/broker/uptime", false},
		{"other/x", false},
		// A bare root without the slash is not a device topic; the
		// prefix check must include the separator so that e.g. a
		// "shelliesplus/x" tree cannot smuggle itself in.
		{"carvilon", false},
		{"shellies", false},
		{"shelliesplus/x", false},
		{"", false},
	}
	for _, c := range cases {
		if got := monitoredTopic(c.topic); got != c.want {
			t.Errorf("monitoredTopic(%q) = %v, want %v", c.topic, got, c.want)
		}
	}
}

// TestMonitorReceivesShelliesTraffic closes the Gen1 loop the way
// TestBrokerConsoleReceivesRealTraffic does for the console: a real
// client publish under shellies/ flows through the mochi monitor hook
// into the Monitor hub (what the device page's SSE renders), while a
// publish outside the device trees is filtered out. The unrelated
// topic is published FIRST on the same connection, so ordering
// guarantees that if it had been forwarded it would arrive before the
// shellies message we wait for.
func TestMonitorReceivesShelliesTraffic(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.CreateDevice(ctx, "dev1", "password123", ""); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	// A Gen1 Shelly publishes under shellies/<id>/..., outside the
	// device's default carvilon/<user>/# subtree, so provisioning
	// grants an explicit ACL; mirror that here (plus one for the
	// unmonitored control topic). Rules are added before Start so the
	// initial authz snapshot carries them.
	if err := store.AddACL(ctx, "dev1", "publish", "shellies/#", true); err != nil {
		t.Fatalf("AddACL shellies: %v", err)
	}
	if err := store.AddACL(ctx, "dev1", "publish", "other/#", true); err != nil {
		t.Fatalf("AddACL other: %v", err)
	}
	m, tcpPort, _ := startManager(t, store)

	msgs, cancel := m.Monitor().Subscribe(64)
	defer cancel()

	c := connect(t, fmt.Sprintf("tcp://127.0.0.1:%d", tcpPort), "dev1", "password123", nil)
	if c == nil {
		t.Fatal("connect failed")
	}
	defer c.Disconnect(100)
	c.Publish("other/x", 0, false, "noise").WaitTimeout(2 * time.Second)
	c.Publish("shellies/shelly1-abc123/relay/0", 0, false, "on").WaitTimeout(2 * time.Second)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-msgs:
			if strings.HasPrefix(msg.Topic, "other/") {
				t.Fatalf("monitor forwarded unmonitored topic %q", msg.Topic)
			}
			if msg.Topic == "shellies/shelly1-abc123/relay/0" {
				if msg.Payload != "on" {
					t.Fatalf("payload = %q, want on", msg.Payload)
				}
				return
			}
		case <-deadline:
			t.Fatal("shellies/ publish did not reach the monitor hub")
		}
	}
}

// TestTCPServerAddrNotRunning proves the plaintext advert fails closed
// before the broker ever started: provisioning a Gen1 device must not
// receive an address for listeners that do not exist.
func TestTCPServerAddrNotRunning(t *testing.T) {
	m := New(newStore(t), NewConsole(50), discardLogger(), t.TempDir(), Settings{
		Enabled: true,
		LANHost: "127.0.0.1",
		TCPPort: freePort(t),
		TLSHost: "127.0.0.1",
		TLSPort: freePort(t),
	})
	if addr, ok := m.TCPServerAddr(); ok {
		t.Fatalf("TCPServerAddr on a never-started broker = (%q, true), want ok=false", addr)
	}
	if addr, ok := m.TLSServerAddr(); ok {
		t.Fatalf("TLSServerAddr on a never-started broker = (%q, true), want ok=false", addr)
	}
}

// TestTCPServerAddrLoopbackRefused pins the loopback-refusal rule on a
// genuinely running broker: advertising 127.0.0.1 to an external Gen1
// device would point the device at itself, so the plaintext advert must
// fail while the TLS advert (which has no such rule) still works - the
// refusal is per-listener policy, not "broker down".
func TestTCPServerAddrLoopbackRefused(t *testing.T) {
	m, _, tlsPort := startManager(t, newStore(t))

	if addr, ok := m.TCPServerAddr(); ok {
		t.Fatalf("TCPServerAddr with loopback advert host = (%q, true), want ok=false", addr)
	}
	tlsAddr, ok := m.TLSServerAddr()
	if !ok {
		t.Fatal("TLSServerAddr should stay available on the same running broker")
	}
	if want := fmt.Sprintf("127.0.0.1:%d", tlsPort); tlsAddr != want {
		t.Fatalf("TLSServerAddr = %q, want %q", tlsAddr, want)
	}
}

// TestTCPServerAddrLANHost covers the happy path: with a non-loopback
// LAN advert host the plaintext advert is host:port of the TCP
// listener. A TEST-NET address cannot actually be bound on the test
// machine, so the broker is started on loopback and the recorded
// advert host is swapped under the Manager's own lock to what Start
// would have stored for a LAN bind - TCPServerAddr is a pure snapshot
// read, so this exercises exactly its decision logic.
func TestTCPServerAddrLANHost(t *testing.T) {
	m, tcpPort, tlsPort := startManager(t, newStore(t))

	m.mu.Lock()
	m.advertHost = "192.0.2.10" // RFC 5737 TEST-NET-1, never a real device
	m.mu.Unlock()

	addr, ok := m.TCPServerAddr()
	if !ok {
		t.Fatal("TCPServerAddr with a LAN advert host should be available")
	}
	if want := fmt.Sprintf("192.0.2.10:%d", tcpPort); addr != want {
		t.Fatalf("TCPServerAddr = %q, want %q", addr, want)
	}
	tlsAddr, ok := m.TLSServerAddr()
	if !ok {
		t.Fatal("TLSServerAddr with a LAN advert host should be available")
	}
	if want := fmt.Sprintf("192.0.2.10:%d", tlsPort); tlsAddr != want {
		t.Fatalf("TLSServerAddr = %q, want %q", tlsAddr, want)
	}

	// IPv6 loopback is loopback too: the refusal rule must not be
	// bypassable by spelling the same interface differently.
	m.mu.Lock()
	m.advertHost = "::1"
	m.mu.Unlock()
	if addr, ok := m.TCPServerAddr(); ok {
		t.Fatalf("TCPServerAddr with ::1 advert host = (%q, true), want ok=false", addr)
	}
}
