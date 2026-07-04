package console

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// ---- guard ----

func TestLanIP(t *testing.T) {
	allowed := []string{"192.168.1.10", "10.0.0.1", "172.16.5.5", "127.0.0.1", "169.254.1.1", "fe80::1", "fc00::1", "::1"}
	for _, s := range allowed {
		if !lanIP(net.ParseIP(s)) {
			t.Errorf("lanIP(%s) = false, want true", s)
		}
	}
	blocked := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2001:4860:4860::8888"}
	for _, s := range blocked {
		if lanIP(net.ParseIP(s)) {
			t.Errorf("lanIP(%s) = true, want false", s)
		}
	}
	// Cloud metadata endpoints sit inside link-local / ULA but must be
	// blocked so a cloud-hosted instance cannot be used for IMDS SSRF.
	for _, s := range []string{"169.254.169.254", "fd00:ec2::254"} {
		if lanIP(net.ParseIP(s)) {
			t.Errorf("lanIP(%s) = true, want false (metadata endpoint)", s)
		}
	}
	// APIPA link-local other than the metadata address stays allowed.
	if !lanIP(net.ParseIP("169.254.1.1")) {
		t.Error("lanIP(169.254.1.1) = false, want true (APIPA)")
	}
}

func TestResolveGuardedBlocksMetadata(t *testing.T) {
	if _, err := resolveGuarded("169.254.169.254", false, time.Second); !errors.Is(err, ErrPublicTarget) {
		t.Fatalf("metadata literal: err = %v, want ErrPublicTarget", err)
	}
}

func TestResolveGuardedPublicBlocked(t *testing.T) {
	if _, err := resolveGuarded("8.8.8.8", false, time.Second); !errors.Is(err, ErrPublicTarget) {
		t.Fatalf("public literal: err = %v, want ErrPublicTarget", err)
	}
	ip, err := resolveGuarded("8.8.8.8", true, time.Second)
	if err != nil || ip.String() != "8.8.8.8" {
		t.Fatalf("allowPublic: ip=%v err=%v", ip, err)
	}
	if _, err := resolveGuarded("", false, time.Second); err == nil {
		t.Fatal("empty host: want error")
	}
}

func TestDialTCPConsolePublicBlocked(t *testing.T) {
	// The guard fires before any dial, so this touches no network.
	if _, err := DialTCPConsole(NetConfig{Host: "8.8.8.8", Port: 80}); !errors.Is(err, ErrPublicTarget) {
		t.Fatalf("err = %v, want ErrPublicTarget", err)
	}
	if _, err := OpenUDPConsole(UDPConfig{Host: "2001:4860:4860::8888", Port: 53}); !errors.Is(err, ErrPublicTarget) {
		t.Fatalf("udp err = %v, want ErrPublicTarget", err)
	}
}

// ---- tcp ----

// echoListener runs a one-connection TCP echo server and returns its
// address. It stops echoing when the peer closes.
func echoListener(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

// readWithDeadline runs one backend Read in a goroutine so a hang fails
// the test instead of wedging it.
func readWithDeadline(t *testing.T, b Backend, d time.Duration) ([]byte, error) {
	t.Helper()
	type res struct {
		p   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		buf := make([]byte, 64*1024)
		n, err := b.Read(buf)
		ch <- res{append([]byte(nil), buf[:n]...), err}
	}()
	select {
	case r := <-ch:
		return r.p, r.err
	case <-time.After(d):
		t.Fatal("backend Read did not return in time")
		return nil, nil
	}
}

func TestTCPEchoRoundtrip(t *testing.T) {
	host, port := echoListener(t)
	b, err := DialTCPConsole(NetConfig{Host: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, err := b.Write([]byte("hello line\n")); err != nil {
		t.Fatal(err)
	}
	got, err := readWithDeadline(t, b, 5*time.Second)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello line\n" {
		t.Fatalf("echo = %q", got)
	}
	// Resize must be a harmless no-op for sockets.
	if err := b.Resize(80, 24); err != nil {
		t.Fatalf("resize: %v", err)
	}
	// Close is idempotent and unblocks a pending read.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTCPDialRefused(t *testing.T) {
	// Grab a port that is free right now, close it, and dial it: nothing
	// listens there, so the dial must fail cleanly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if _, err := DialTCPConsole(NetConfig{Host: "127.0.0.1", Port: port, Timeout: 3 * time.Second}); err == nil {
		t.Fatal("dial to closed port: want error")
	}
}

func TestTCPBadPort(t *testing.T) {
	for _, p := range []int{0, -1, 70000} {
		if _, err := DialTCPConsole(NetConfig{Host: "127.0.0.1", Port: p}); err == nil {
			t.Fatalf("port %d: want error", p)
		}
	}
}

// TestTCPServeTeardown runs a real TCP backend through the manager's
// Serve bridge and checks the step-1 teardown guarantee holds for the
// new backend type: the client vanishing tears everything down, no
// session stays registered.
func TestTCPServeTeardown(t *testing.T) {
	host, port := echoListener(t)
	b, err := DialTCPConsole(NetConfig{Host: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil)
	s, err := m.Open("tcp", "tcp test", "tester", b)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeTransport()
	served := make(chan struct{})
	go func() { m.Serve(t.Context(), s, f); close(served) }()

	f.in <- ClientMsg{Type: MsgData, Data: []byte("ping\n")}
	select {
	case out := <-f.out:
		if string(out) != "ping\n" {
			t.Errorf("echo through bridge = %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no echo through the bridge")
	}

	_ = f.Close() // browser goes away
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after transport close")
	}
	if n := m.Count(); n != 0 {
		t.Fatalf("sessions left after teardown: %d", n)
	}
}

// ---- udp ----

// readUDPLine collects stream bytes from the backend until one full
// JSON line is available and returns it decoded.
func readUDPLine(t *testing.T, b Backend, buf *strings.Builder) udpLine {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s := buf.String(); strings.Contains(s, "\n") {
			line := s[:strings.Index(s, "\n")]
			rest := s[strings.Index(s, "\n")+1:]
			buf.Reset()
			buf.WriteString(rest)
			var l udpLine
			if err := json.Unmarshal([]byte(line), &l); err != nil {
				t.Fatalf("bad stream line %q: %v", line, err)
			}
			return l
		}
		if time.Now().After(deadline) {
			t.Fatal("no udp stream line in time")
		}
		p, err := readWithDeadline(t, b, 5*time.Second)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		buf.Write(p)
	}
}

func TestUDPRoundtrip(t *testing.T) {
	// Peer socket: plays the LAN device on the other side.
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port

	b, err := OpenUDPConsole(UDPConfig{Host: "127.0.0.1", Port: peerPort})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	var acc strings.Builder
	first := readUDPLine(t, b, &acc)
	if first.Sys != "listening" || first.Port == 0 {
		t.Fatalf("first line = %+v, want sys=listening with port", first)
	}

	// send: one Write = one datagram at the peer.
	if _, err := b.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	dbuf := make([]byte, 2048)
	n, from, err := peer.ReadFromUDP(dbuf)
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if string(dbuf[:n]) != "ping" {
		t.Fatalf("peer got %q", dbuf[:n])
	}
	if from.Port != first.Port {
		t.Fatalf("datagram source port %d, backend reported %d", from.Port, first.Port)
	}

	// receive: a datagram at the backend port surfaces with its sender.
	if _, err := peer.WriteToUDP([]byte("pong"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: first.Port}); err != nil {
		t.Fatal(err)
	}
	got := readUDPLine(t, b, &acc)
	if got.From == "" || !strings.HasPrefix(got.From, "127.0.0.1:") {
		t.Fatalf("received line sender = %q", got.From)
	}
	payload, err := base64.StdEncoding.DecodeString(got.B64)
	if err != nil || string(payload) != "pong" {
		t.Fatalf("received payload = %q (%v)", payload, err)
	}
}

func TestUDPListenPortReceives(t *testing.T) {
	// Reserve a concrete port the way OS tests usually do: bind :0, note
	// the port, release, and let the backend bind it explicitly.
	probe, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	_ = probe.Close()

	b, err := OpenUDPConsole(UDPConfig{ListenPort: port})
	if err != nil {
		t.Skipf("could not rebind reserved port %d: %v", port, err)
	}
	defer b.Close()

	var acc strings.Builder
	first := readUDPLine(t, b, &acc)
	if first.Sys != "listening" || first.Port != port {
		t.Fatalf("first line = %+v, want listening on %d", first, port)
	}

	sender, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	if _, err := sender.Write([]byte("beacon")); err != nil {
		t.Fatal(err)
	}
	got := readUDPLine(t, b, &acc)
	payload, _ := base64.StdEncoding.DecodeString(got.B64)
	if string(payload) != "beacon" || got.From == "" {
		t.Fatalf("line = %+v payload=%q", got, payload)
	}

	// Sending without a target must neither error nor kill the session.
	if n, err := b.Write([]byte("dropped")); err != nil || n != len("dropped") {
		t.Fatalf("write without target: n=%d err=%v", n, err)
	}
}

func TestUDPEmptyDatagramVisible(t *testing.T) {
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port

	b, err := OpenUDPConsole(UDPConfig{Host: "127.0.0.1", Port: peerPort})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var acc strings.Builder
	first := readUDPLine(t, b, &acc)

	// A zero-byte datagram is a legal, visible event.
	if _, err := peer.WriteToUDP(nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: first.Port}); err != nil {
		t.Fatal(err)
	}
	got := readUDPLine(t, b, &acc)
	if got.From == "" {
		t.Fatalf("empty datagram produced no sender: %+v", got)
	}
	payload, err := base64.StdEncoding.DecodeString(got.B64)
	if err != nil || len(payload) != 0 {
		t.Fatalf("empty datagram payload = %q (%v)", payload, err)
	}
}

func TestUDPConfigValidation(t *testing.T) {
	if _, err := OpenUDPConsole(UDPConfig{}); err == nil {
		t.Fatal("no listen, no target: want error")
	}
	if _, err := OpenUDPConsole(UDPConfig{Host: "127.0.0.1"}); err == nil {
		t.Fatal("target host without port: want error")
	}
	if _, err := OpenUDPConsole(UDPConfig{ListenPort: 70000}); err == nil {
		t.Fatal("listen port out of range: want error")
	}
}

func TestUDPCloseUnblocksRead(t *testing.T) {
	b, err := OpenUDPConsole(UDPConfig{Host: "127.0.0.1", Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	var acc strings.Builder
	_ = readUDPLine(t, b, &acc) // drain the listening line
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		_, rerr := b.Read(buf)
		done <- rerr
	}()
	time.Sleep(50 * time.Millisecond)
	_ = b.Close()
	select {
	case rerr := <-done:
		if rerr == nil {
			t.Fatal("read after close: want error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Read")
	}
}
