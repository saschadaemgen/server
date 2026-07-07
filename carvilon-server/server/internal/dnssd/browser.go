package dnssd

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

// mdnsGroup is the IPv4 mDNS destination (224.0.0.251:5353).
var mdnsGroup = &net.UDPAddr{IP: net.ParseIP(mdnsIPv4), Port: mdnsPort}

// Source is the seam the discovery coordinator consumes: a stream of seen
// service instances plus a way to actively provoke fresh responses. The
// real implementation is *Browser; tests inject a fake.
type Source interface {
	Entries() <-chan Entry
	Scan(ctx context.Context)
}

// Mode describes how much of the browser actually came up.
type Mode int

const (
	// ModeOff - no socket at all (both binds failed). Scan is a no-op.
	ModeOff Mode = iota
	// ModePassive - bound 5353 and joined the group: passive announcements
	// are heard AND active queries go out multicast.
	ModePassive
	// ModeActiveOnly - could not bind 5353 (e.g. avahi without reuse); only
	// on-demand/periodic active queries from an ephemeral port work, with
	// the QU bit so responses come back by unicast.
	ModeActiveOnly
)

func (m Mode) String() string {
	switch m {
	case ModePassive:
		return "passive"
	case ModeActiveOnly:
		return "active-only"
	default:
		return "off"
	}
}

// Browser is a running DNS-SD browser for one service type.
type Browser struct {
	service string
	log     *slog.Logger
	mode    Mode

	entries chan Entry

	conn   net.PacketConn   // the passive 5353 socket (ModePassive)
	pconn  *ipv4.PacketConn // ipv4 view of conn, for group + interface control
	ifaces []net.Interface  // interfaces the query is sent out on

	closeOnce sync.Once
	closed    chan struct{}
}

// Open starts a browser for service (e.g. "_shelly._tcp.local"). It never
// returns nil: on a total socket failure it returns a ModeOff browser whose
// Scan is a no-op, so the caller degrades cleanly instead of crashing. The
// chosen mode is logged (no addresses - only the coarse mode).
func Open(service string, log *slog.Logger) *Browser {
	if log == nil {
		log = slog.Default()
	}
	b := &Browser{
		service: service,
		log:     log,
		entries: make(chan Entry, 64),
		closed:  make(chan struct{}),
	}

	if conn, pconn, ifaces, err := openPassive(); err == nil {
		b.conn, b.pconn, b.ifaces, b.mode = conn, pconn, ifaces, ModePassive
		go b.readLoop()
		b.log.Info("dnssd browser listening", "component", "shelly-discovery", "mode", b.mode.String(), "service", service)
		return b
	} else {
		// Passive bind failed (very likely a system responder holds 5353
		// without allowing reuse) - fall back to active-only queries.
		b.mode = ModeActiveOnly
		b.ifaces = multicastInterfaces()
		b.log.Warn("dnssd passive listen unavailable; using active queries only",
			"component", "shelly-discovery", "service", service)
	}
	return b
}

// openPassive binds UDP 5353 with address/port reuse and joins the mDNS
// group on every eligible interface.
func openPassive() (net.PacketConn, *ipv4.PacketConn, []net.Interface, error) {
	lc := net.ListenConfig{Control: reuseControl}
	conn, err := lc.ListenPacket(context.Background(), "udp4", ":5353")
	if err != nil {
		return nil, nil, nil, err
	}
	p := ipv4.NewPacketConn(conn)
	_ = p.SetMulticastLoopback(true) // so a local announcer (tests, self) is heard
	_ = p.SetMulticastTTL(255)       // mDNS convention

	ifaces := multicastInterfaces()
	joined := ifaces[:0:0]
	for _, ifi := range ifaces {
		ifi := ifi
		if err := p.JoinGroup(&ifi, mdnsGroup); err == nil {
			joined = append(joined, ifi)
		}
	}
	if len(joined) == 0 {
		// Bound but joined nothing useful: treat as a failed passive open so
		// the caller uses active-only rather than a deaf socket.
		_ = conn.Close()
		return nil, nil, nil, errNoJoin
	}
	return conn, p, joined, nil
}

var errNoJoin = errClose("dnssd: joined no multicast interface")

type errClose string

func (e errClose) Error() string { return string(e) }

// Entries returns the channel of seen instances (passive announcements +
// query responses). Never closed while the browser lives; drained by the
// coordinator.
func (b *Browser) Entries() <-chan Entry { return b.entries }

// Mode reports how much of the browser came up.
func (b *Browser) Mode() Mode { return b.mode }

// Scan actively provokes fresh responses. In passive mode it multicasts a
// PTR query out every joined interface (answers arrive on the read loop).
// In active-only mode it opens a short-lived ephemeral socket, sends a
// unicast-requested query and reads the direct responses. ModeOff is a
// no-op. Safe to call concurrently with the passive read loop.
func (b *Browser) Scan(ctx context.Context) {
	switch b.mode {
	case ModePassive:
		b.multicastQuery()
	case ModeActiveOnly:
		b.ephemeralQuery(ctx)
	}
}

// multicastQuery sends a QM (multicast-response) PTR query out each joined
// interface; the passive read loop collects the answers.
func (b *Browser) multicastQuery() {
	query, err := buildQuery(b.service, false)
	if err != nil {
		return
	}
	if len(b.ifaces) == 0 {
		_, _ = b.pconn.WriteTo(query, nil, mdnsGroup)
		return
	}
	for _, ifi := range b.ifaces {
		cm := &ipv4.ControlMessage{IfIndex: ifi.Index}
		if _, err := b.pconn.WriteTo(query, cm, mdnsGroup); err != nil {
			// Retry once without the interface hint - some stacks reject the
			// control message but still route the packet on their own.
			_, _ = b.pconn.WriteTo(query, nil, mdnsGroup)
		}
	}
}

// ephemeralQuery is the 5353-free fallback: bind an ephemeral UDP port, send
// a QU (unicast-response) query to the group, and read the direct responses
// for a short window. Needs no 5353 access, so it coexists with avahi.
func (b *Browser) ephemeralQuery(ctx context.Context) {
	query, err := buildQuery(b.service, true)
	if err != nil {
		return
	}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return
	}
	defer conn.Close()
	p := ipv4.NewPacketConn(conn)
	_ = p.SetMulticastTTL(255)

	ifaces := b.ifaces
	if len(ifaces) == 0 {
		_, _ = conn.WriteTo(query, mdnsGroup)
	} else {
		for _, ifi := range ifaces {
			cm := &ipv4.ControlMessage{IfIndex: ifi.Index}
			if _, err := p.WriteTo(query, cm, mdnsGroup); err != nil {
				_, _ = conn.WriteTo(query, mdnsGroup)
			}
		}
	}

	deadline := time.Now().Add(1500 * time.Millisecond)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, maxPacket)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.closed:
			return
		default:
		}
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return // deadline reached or socket closed
		}
		for _, e := range parseEntries(buf[:n], b.service) {
			b.emit(e)
		}
	}
}

// readLoop is the passive receiver: every multicast frame on 5353 is parsed
// and its instances emitted. It exits when the socket is closed.
func (b *Browser) readLoop() {
	buf := make([]byte, maxPacket)
	for {
		n, _, err := b.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-b.closed:
				return // normal shutdown
			default:
			}
			// A persistent read error means the socket is unusable; stop the
			// loop (the periodic active Scan still works via ephemeral ports).
			b.log.Warn("dnssd read loop stopped", "component", "shelly-discovery")
			return
		}
		for _, e := range parseEntries(buf[:n], b.service) {
			b.emit(e)
		}
	}
}

// emit hands one entry to the coordinator without ever blocking the read
// loop: if the consumer is momentarily behind, the (redundant, periodically
// re-sent) announcement is dropped rather than stalling the socket.
func (b *Browser) emit(e Entry) {
	select {
	case b.entries <- e:
	case <-b.closed:
	default:
	}
}

// Close stops the browser and releases the socket. Idempotent; nil-safe.
func (b *Browser) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		close(b.closed)
		if b.pconn != nil {
			for _, ifi := range b.ifaces {
				ifi := ifi
				_ = b.pconn.LeaveGroup(&ifi, mdnsGroup)
			}
		}
		if b.conn != nil {
			_ = b.conn.Close()
		}
	})
	return nil
}

// multicastInterfaces returns the up, multicast-capable interfaces that
// carry an IPv4 address - the ones worth joining/sending on.
func multicastInterfaces() []net.Interface {
	ifis, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, ifi := range ifis {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ifi)
				break
			}
		}
	}
	return out
}
