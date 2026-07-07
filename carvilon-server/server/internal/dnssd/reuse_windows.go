//go:build windows

package dnssd

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// reuseControl sets SO_REUSEADDR on Windows. Windows has no SO_REUSEPORT;
// its SO_REUSEADDR already permits multiple sockets to bind the same
// UDP port (the semantics differ from unix but are sufficient to coexist
// with another mDNS listener). Development runs on Windows; the RPi
// deployment where avahi coexistence really matters uses the unix path.
func reuseControl(network, address string, c syscall.RawConn) error {
	var opErr error
	if err := c.Control(func(fd uintptr) {
		if err := windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1); err != nil {
			opErr = err
		}
	}); err != nil {
		return err
	}
	return opErr
}
