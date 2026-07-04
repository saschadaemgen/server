// TCP and UDP console backends (Terminal-Track step 2): plain Backend
// adapters on the session frame — the browser cannot open raw sockets,
// so the Go server bridges pane <-> WebSocket <-> socket.
//
// The server deliberately does NOT become an open relay: targets are
// restricted to private/LAN address ranges (RFC1918 / ULA, link-local,
// loopback) by default. The guard resolves the host itself and dials the
// vetted IP, so a DNS name cannot re-resolve to a public address between
// check and dial. AllowPublic exists for a deliberate operator decision
// in code — nothing in the product sets it.
//
// TCP is a raw byte stream in both directions. UDP has datagram
// boundaries and a per-datagram sender the UI must show, but the frame
// streams plain bytes — so the UDP backend emits one JSON line per
// event into the stream ({"from":"ip:port","b64":"..."} for a received
// datagram, {"sys":...} for lifecycle notes) and the console UI decodes
// them. One client binary frame = one Write = one sent datagram.
package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// ErrPublicTarget is returned when a console target resolves only to
// addresses outside the allowed LAN ranges.
var ErrPublicTarget = errors.New("console: target is outside the LAN (public targets are blocked)")

// NetConfig describes one outbound TCP console connection.
type NetConfig struct {
	Host    string
	Port    int
	Timeout time.Duration
	// AllowPublic lifts the LAN-only target guard. Never set by the
	// product; a deliberate operator decision in code only.
	AllowPublic bool
}

// UDPConfig describes one UDP console: bind a local listen port and/or
// send to a target host:port. At least one of the two must be given.
type UDPConfig struct {
	ListenPort int
	Host       string
	Port       int
	Timeout    time.Duration
	// AllowPublic — see NetConfig.
	AllowPublic bool
}

// metadataIPs are the cloud instance-metadata endpoints (IMDS). They sit
// inside link-local / ULA, so the LAN test would otherwise wave them
// through — but if this binary ever runs on a cloud VM, letting an admin
// point the console at them is a direct credential-exfil SSRF. On the
// home-LAN edge device they are never real targets anyway, so blocking
// them costs nothing.
var metadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"), // AWS/GCP/Azure/OpenStack/DO IMDS
	net.ParseIP("fd00:ec2::254"),   // AWS IPv6 IMDS
}

// lanIP reports whether ip is an allowed console target: a private
// (RFC1918 / ULA fc00::/7), link-local, or loopback address — but never
// a cloud metadata endpoint (see metadataIPs).
func lanIP(ip net.IP) bool {
	for _, m := range metadataIPs {
		if ip.Equal(m) {
			return false
		}
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

// resolveGuarded resolves host and returns the first address inside the
// allowed LAN ranges (with allowPublic: the first address, period). The
// vetted IP — not the name — is what gets dialed afterwards.
func resolveGuarded(host string, allowPublic bool, timeout time.Duration) (net.IP, error) {
	if host == "" {
		return nil, errors.New("console: target host missing")
	}
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	if ip := net.ParseIP(host); ip != nil {
		if allowPublic || lanIP(ip) {
			return ip, nil
		}
		return nil, ErrPublicTarget
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("console: resolve %s: %w", host, err)
	}
	for _, a := range addrs {
		if allowPublic || lanIP(a.IP) {
			return a.IP, nil
		}
	}
	return nil, ErrPublicTarget
}

func checkPort(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("console: invalid port %d", port)
	}
	return nil
}

// ---- TCP ----

// DialTCPConsole connects to a (LAN-guarded) TCP target and returns the
// live connection as a Backend. Read/Write are the raw byte stream;
// Resize is a no-op — sockets have no window.
func DialTCPConsole(cfg NetConfig) (Backend, error) {
	if err := checkPort(cfg.Port); err != nil {
		return nil, err
	}
	ip, err := resolveGuarded(cfg.Host, cfg.AllowPublic, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(cfg.Port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("console: tcp dial %s: %w", addr, err)
	}
	return &tcpBackend{conn: conn}, nil
}

// tcpBackend wraps the live TCP connection: the raw stream plus an
// idempotent Close (the frame and CloseAll may both close).
type tcpBackend struct {
	conn      net.Conn
	closeOnce sync.Once
}

func (b *tcpBackend) Read(p []byte) (int, error) { return b.conn.Read(p) }

// tcpWriteTimeout bounds a single Write. The frame calls Write
// synchronously from its client-pump goroutine and only tears the
// session down once that goroutine returns; a bare write to a peer that
// has stalled its receive window would block there forever, wedging the
// session in its slot until server shutdown. A generous per-write
// deadline lets a genuinely stuck send eventually error, unblock the
// pump, and free the slot.
const tcpWriteTimeout = 30 * time.Second

func (b *tcpBackend) Write(p []byte) (int, error) {
	_ = b.conn.SetWriteDeadline(time.Now().Add(tcpWriteTimeout))
	return b.conn.Write(p)
}
func (b *tcpBackend) Resize(cols, rows uint16) error {
	return nil // sockets have no window
}
func (b *tcpBackend) Close() error {
	b.closeOnce.Do(func() { _ = b.conn.Close() })
	return nil
}

// ---- UDP ----

// udpLine is one event the UDP backend feeds into the byte stream, one
// JSON object per line. A received datagram carries From (+B64, which may
// be "" for a legal empty datagram, so it is NOT omitempty); a lifecycle
// note carries Sys.
type udpLine struct {
	From string `json:"from,omitempty"` // sender ip:port of a received datagram
	B64  string `json:"b64,omitempty"`  // datagram payload, base64 ("" for an empty datagram)
	Sys  string `json:"sys,omitempty"`  // lifecycle note ("listening")
	Port int    `json:"port,omitempty"` // with Sys "listening": the bound local port
}

// OpenUDPConsole binds a UDP socket and returns it as a Backend. With
// ListenPort > 0 the socket listens there; without it an ephemeral port
// is bound (replies to sent datagrams still arrive). The socket binds
// every interface (LAN devices reach it there); inbound datagrams whose
// sender is outside the LAN are dropped unless AllowPublic, matching the
// outbound target guard. With a target host:port, each client Write
// sends one datagram there; without a target, writes are dropped
// silently (the UI disables sending then).
func OpenUDPConsole(cfg UDPConfig) (Backend, error) {
	hasTarget := cfg.Host != "" || cfg.Port != 0
	if !hasTarget && cfg.ListenPort <= 0 {
		return nil, errors.New("console: udp needs a listen port and/or a target")
	}
	if cfg.ListenPort < 0 || cfg.ListenPort > 65535 {
		return nil, fmt.Errorf("console: invalid listen port %d", cfg.ListenPort)
	}

	var target *net.UDPAddr
	if hasTarget {
		if err := checkPort(cfg.Port); err != nil {
			return nil, err
		}
		ip, err := resolveGuarded(cfg.Host, cfg.AllowPublic, cfg.Timeout)
		if err != nil {
			return nil, err
		}
		target = &net.UDPAddr{IP: ip, Port: cfg.Port}
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.ListenPort})
	if err != nil {
		return nil, fmt.Errorf("console: udp bind :%d: %w", cfg.ListenPort, err)
	}

	b := &udpBackend{conn: conn, target: target, allowPublic: cfg.AllowPublic}
	// First stream line tells the UI (and tests) the actually bound port —
	// meaningful when ListenPort was 0 (ephemeral, send-only setups).
	if la, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		b.queueLine(udpLine{Sys: "listening", Port: la.Port})
	}
	return b, nil
}

// udpBackend adapts the datagram socket to the frame's stream Backend.
// Read is only ever called sequentially by the frame's single output
// goroutine, so pending needs no lock; Write/Close come from other
// goroutines and touch only conn/target/allowPublic, all immutable after
// construction.
type udpBackend struct {
	conn        *net.UDPConn
	target      *net.UDPAddr
	allowPublic bool
	pending     []byte // queued stream bytes not yet handed to Read

	closeOnce sync.Once
	dbuf      [64 * 1024]byte // fits the max UDP payload (65507) without truncation
}

// queueLine appends one JSON line to the pending stream output.
func (b *udpBackend) queueLine(l udpLine) {
	j, err := json.Marshal(l)
	if err != nil {
		return // a marshal failure must never kill the session
	}
	b.pending = append(b.pending, j...)
	b.pending = append(b.pending, '\n')
}

// Read hands out pending lines first, then blocks for the next datagram
// and queues it as a JSON line (empty datagrams included — a 0-byte
// keepalive is a visible event on a wire-diagnostics console). Datagrams
// from a non-LAN sender are dropped unless AllowPublic. Close unblocks
// the blocking read.
func (b *udpBackend) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		n, addr, err := b.conn.ReadFromUDP(b.dbuf[:])
		if err != nil {
			return 0, err
		}
		if addr != nil && (b.allowPublic || lanIP(addr.IP)) {
			b.queueLine(udpLine{From: addr.String(), B64: base64.StdEncoding.EncodeToString(b.dbuf[:n])})
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

// Write sends p as one datagram to the configured target. Without a
// target the datagram is dropped (the UI disables sending then); send
// errors are swallowed too — a Write error would tear the whole session
// down, and transient ICMP unreachable noise must not do that.
func (b *udpBackend) Write(p []byte) (int, error) {
	if b.target != nil {
		_, _ = b.conn.WriteToUDP(p, b.target)
	}
	return len(p), nil
}

func (b *udpBackend) Resize(cols, rows uint16) error { return nil }

func (b *udpBackend) Close() error {
	b.closeOnce.Do(func() { _ = b.conn.Close() })
	return nil
}
