// Package discovery implements stage 1 of the UA Intercom Viewer
// mock daemon: a UDP listener on port 10001 that accepts UDM probes
// (multicast 233.89.188.1, broadcast, or unicast) and replies
// unicast with a TLV-encoded discovery response.
//
// Multi-mock safe via SO_REUSEPORT.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	"carvilon.local/mock/internal/identity"
)

// Logger is the minimal logging surface this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Listener listens on UDP :10001 for UniFi discovery probes and
// replies unicast with a TLV-encoded discovery response. The
// response is pre-built once at construction time.
type Listener struct {
	identity  *identity.MockIdentity
	conn      *net.UDPConn
	log       Logger
	response  []byte
	closeOnce sync.Once
}

// New constructs a Listener bound to UDP :10001 on all interfaces
// with SO_REUSEPORT enabled, joins the discovery multicast group
// on every viable IPv4 interface, and stores the identity to
// build response payloads from.
func New(id *identity.MockIdentity, log Logger) (*Listener, error) {
	if id == nil {
		return nil, errors.New("discovery: identity must not be nil")
	}
	if log == nil {
		return nil, errors.New("discovery: logger must not be nil")
	}

	response, err := buildResponse(id)
	if err != nil {
		return nil, fmt.Errorf("discovery: build response: %w", err)
	}

	lc := net.ListenConfig{
		Control: setReusePort,
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":10001")
	if err != nil {
		return nil, fmt.Errorf("discovery: listen :10001: %w", err)
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, fmt.Errorf("discovery: expected *net.UDPConn, got %T", pc)
	}

	joined, err := joinMulticastAll(conn)
	if err != nil {
		log.Warnf("discovery: multicast join: %v", err)
	}
	log.Infof("discovery: joined multicast on %d interface(s)", joined)

	return &Listener{
		identity: id,
		conn:     conn,
		log:      log,
		response: response,
	}, nil
}

// Run blocks until ctx is cancelled or Close is called. Per-packet
// errors are logged and the loop continues.
func (l *Listener) Run(ctx context.Context) error {
	l.log.Infof("discovery: listening on %s for %s", l.conn.LocalAddr(), l.identity.Name)
	buf := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_ = l.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, src, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			l.log.Errorf("discovery: read: %v", err)
			continue
		}
		if !isDiscoveryProbe(buf[:n]) {
			previewLen := min(n, 16)
			l.log.Warnf("discovery: non-probe %d bytes from %s: % x", n, src, buf[:previewLen])
			continue
		}
		if _, err := l.conn.WriteToUDP(l.response, src); err != nil {
			l.log.Errorf("discovery: reply to %s: %v", src, err)
			continue
		}
		l.log.Infof("discovery: replied to %s with %d-byte response", src, len(l.response))
	}
}

// Close releases the socket. Safe to call multiple times.
func (l *Listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		if l.conn != nil {
			err = l.conn.Close()
		}
	})
	return err
}

// joinMulticastAll iterates net.Interfaces() and joins the
// discovery multicast group on every interface that is up,
// multicast-capable, non-loopback, and has at least one IPv4
// address. Returns the count of successful joins. The platform-
// specific join lives in sockopt_*.go.
func joinMulticastAll(conn *net.UDPConn) (int, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("syscall conn: %w", err)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return 0, fmt.Errorf("list interfaces: %w", err)
	}
	var joined int
	var lastErr error
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 ||
			iface.Flags&net.FlagLoopback != 0 ||
			iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		ipv4 := firstIPv4(iface)
		if ipv4 == nil {
			continue
		}
		if err := joinMulticast(rawConn, ipv4); err != nil {
			lastErr = fmt.Errorf("iface %s (%s): %w", iface.Name, ipv4, err)
			continue
		}
		joined++
	}
	if joined == 0 && lastErr != nil {
		return 0, lastErr
	}
	return joined, nil
}

func firstIPv4(iface net.Interface) net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if v4 := ipnet.IP.To4(); v4 != nil {
				return v4
			}
		}
	}
	return nil
}

// rawSetsockoptIntCompat is referenced by sockopt_*.go files for
// setReusePort. Defined here so the platform files only need to
// supply the Linux-specific constant value plus the membership
// helper. Kept private.
var _ = (syscall.RawConn)(nil)
