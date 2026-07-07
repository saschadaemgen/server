// Package dnssd is a minimal, pure-Go DNS-SD (mDNS) service browser built
// directly on golang.org/x/net/dns/dnsmessage and golang.org/x/net/ipv4.
//
// Why a small own client instead of a library: the two hard requirements
// of the Shelly discovery feature - coexisting with a system responder
// (avahi) already holding UDP 5353 via SO_REUSEADDR/SO_REUSEPORT, and
// degrading to periodic active queries from an ephemeral port when 5353
// cannot be bound at all - both live at the socket layer, below what the
// common browsers expose. Owning that thin socket layer is the honest
// answer; the security-critical part (parsing untrusted multicast frames)
// is a pure function that is exhaustively unit-tested, no network needed.
//
// Scope: passive listening for announcements + on-demand and periodic
// active PTR queries for one service type (e.g. "_shelly._tcp"). It
// extracts, per instance, the SRV host/port, the A (IPv4) addresses and
// the TXT key/value pairs. It applies NO trust policy of its own - the
// caller LAN-guards and de-duplicates. IPv6/AAAA is intentionally ignored
// (the Shelly LAN path is IPv4).
package dnssd

import (
	"net/netip"
	"strings"
)

// mDNS well-known group + port (RFC 6762).
const (
	mdnsIPv4  = "224.0.0.251"
	mdnsPort  = 5353
	maxPacket = 9000 // generous mDNS frame ceiling (jumbo-safe)
)

// Entry is one service instance seen in an announcement or query response.
// Any field may be zero: a partial announcement is still surfaced, and the
// caller decides what is usable (an Entry with no LAN address is dropped
// upstream, never here).
type Entry struct {
	Instance string       // full instance name, e.g. "shellyplus1pm-abc._shelly._tcp.local."
	Service  string       // the service type this browser watches
	Host     string       // SRV target host, e.g. "shellyplus1pm-abc.local."
	Addrs    []netip.Addr // A records for Host (IPv4 only)
	Port     uint16       // SRV port
	TXT      map[string]string
}

// InstanceLabel returns the leading label of the instance name (the part
// before the service type) - for Shelly this is the device id such as
// "shellyplus1pm-a8032ab1c2d3". Empty when the name has no label.
func (e Entry) InstanceLabel() string {
	name := strings.TrimSuffix(e.Instance, ".")
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// fqdn ensures a trailing dot so it matches on-wire names.
func fqdn(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "."
	}
	if !strings.HasSuffix(s, ".") {
		return s + "."
	}
	return s
}
