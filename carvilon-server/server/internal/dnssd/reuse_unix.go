//go:build unix

package dnssd

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reuseControl sets SO_REUSEADDR and SO_REUSEPORT on the raw socket before
// bind, so the passive listener can share UDP 5353 with a system mDNS
// responder (avahi / systemd-resolved) that is already bound there. Without
// SO_REUSEPORT the bind would fail with EADDRINUSE on a RPi running avahi -
// exactly the deployment case the briefing calls out. A failure to set the
// option is returned so the caller can fall back to ephemeral-port active
// queries instead of listening.
func reuseControl(network, address string, c syscall.RawConn) error {
	var opErr error
	if err := c.Control(func(fd uintptr) {
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			opErr = err
			return
		}
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			opErr = err
			return
		}
	}); err != nil {
		return err
	}
	return opErr
}
