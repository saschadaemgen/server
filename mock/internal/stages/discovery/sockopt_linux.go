//go:build linux

package discovery

import (
	"fmt"
	"net"
	"syscall"
)

// soReusePort is the Linux SO_REUSEPORT constant value from
// <sys/socket.h>. Hardcoded to avoid pulling in
// golang.org/x/sys/unix. Stable across all current Linux kernels.
const soReusePort = 15

// setReusePort enables SO_REUSEADDR + SO_REUSEPORT on the socket
// fd. Used as the Control func of net.ListenConfig.
func setReusePort(network, address string, c syscall.RawConn) error {
	var opErr error
	err := c.Control(func(fd uintptr) {
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
			opErr = fmt.Errorf("SO_REUSEADDR: %w", e)
			return
		}
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1); e != nil {
			opErr = fmt.Errorf("SO_REUSEPORT: %w", e)
			return
		}
	})
	if err != nil {
		return err
	}
	return opErr
}

// joinMulticast adds membership for the discovery multicast group
// (233.89.188.1) on the interface identified by ifaceIPv4.
func joinMulticast(rawConn syscall.RawConn, ifaceIPv4 net.IP) error {
	mreq := &syscall.IPMreq{}
	copy(mreq.Multiaddr[:], net.IPv4(233, 89, 188, 1).To4())
	copy(mreq.Interface[:], ifaceIPv4.To4())
	var opErr error
	err := rawConn.Control(func(fd uintptr) {
		opErr = syscall.SetsockoptIPMreq(int(fd), syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, mreq)
	})
	if err != nil {
		return err
	}
	return opErr
}
