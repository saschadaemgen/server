//go:build !linux

package discovery

import (
	"net"
	"syscall"
)

// setReusePort is a no-op on non-Linux platforms. The mock can be
// run for development purposes without REUSEPORT, but multi-mock
// requires Linux.
func setReusePort(network, address string, c syscall.RawConn) error {
	return nil
}

// joinMulticast is a no-op on non-Linux platforms. The mock will
// not actually receive multicast probes when run outside of Linux.
func joinMulticast(rawConn syscall.RawConn, ifaceIPv4 net.IP) error {
	return nil
}
