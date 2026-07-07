package dnssd

import (
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

const shellyService = "_shelly._tcp.local"

// mustName panics on an invalid name (test helper only).
func mustName(t *testing.T, s string) dnsmessage.Name {
	t.Helper()
	n, err := dnsmessage.NewName(s)
	if err != nil {
		t.Fatalf("NewName(%q): %v", s, err)
	}
	return n
}

// buildAnnouncement packs a realistic Shelly Gen2 announcement: a PTR from
// the service to the instance, the instance's SRV + TXT, and an A record
// for the SRV target host. Records are spread across answer/additional
// sections to exercise both.
func buildAnnouncement(t *testing.T, instance, host string, ip netip.Addr, port uint16, txt []string) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	b.EnableCompression()
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	// PTR: _shelly._tcp.local -> instance
	if err := b.PTRResource(dnsmessage.ResourceHeader{
		Name: mustName(t, shellyService+"."), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: 120,
	}, dnsmessage.PTRResource{PTR: mustName(t, instance)}); err != nil {
		t.Fatal(err)
	}
	// SRV
	if err := b.SRVResource(dnsmessage.ResourceHeader{
		Name: mustName(t, instance), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET, TTL: 120,
	}, dnsmessage.SRVResource{Priority: 0, Weight: 0, Port: port, Target: mustName(t, host)}); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAdditionals(); err != nil {
		t.Fatal(err)
	}
	// TXT
	if err := b.TXTResource(dnsmessage.ResourceHeader{
		Name: mustName(t, instance), Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 120,
	}, dnsmessage.TXTResource{TXT: txt}); err != nil {
		t.Fatal(err)
	}
	// A
	if err := b.AResource(dnsmessage.ResourceHeader{
		Name: mustName(t, host), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 120,
	}, dnsmessage.AResource{A: ip.As4()}); err != nil {
		t.Fatal(err)
	}
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParseAnnouncement(t *testing.T) {
	ip := netip.MustParseAddr("192.168.1.51")
	pkt := buildAnnouncement(t,
		"shellyplus1pm-a8032ab1c2d3._shelly._tcp.local.",
		"shellyplus1pm-a8032ab1c2d3.local.",
		ip, 80,
		[]string{"gen=2", "app=Plus1PM", "id=shellyplus1pm-a8032ab1c2d3", "ver=1.4.4"},
	)
	entries := parseEntries(pkt, shellyService)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.InstanceLabel() != "shellyplus1pm-a8032ab1c2d3" {
		t.Fatalf("label = %q", e.InstanceLabel())
	}
	if e.Port != 80 {
		t.Fatalf("port = %d, want 80", e.Port)
	}
	if len(e.Addrs) != 1 || e.Addrs[0] != ip {
		t.Fatalf("addrs = %v, want [%v]", e.Addrs, ip)
	}
	if e.TXT["gen"] != "2" || e.TXT["id"] != "shellyplus1pm-a8032ab1c2d3" {
		t.Fatalf("txt = %v", e.TXT)
	}
}

// TestParseForeignServiceIgnored: a printer announcement in the same packet
// must not surface as a Shelly instance.
func TestParseForeignServiceIgnored(t *testing.T) {
	pkt := buildAnnouncement(t,
		"Office Printer._ipp._tcp.local.",
		"printer.local.",
		netip.MustParseAddr("192.168.1.7"), 631,
		[]string{"ty=Some Printer"},
	)
	if entries := parseEntries(pkt, shellyService); len(entries) != 0 {
		t.Fatalf("foreign entries = %d, want 0: %+v", len(entries), entries)
	}
}

// TestParseOffLANAddressSurfaced: parsing does NOT filter addresses (that is
// the caller's LAN guard); it must faithfully surface whatever A record was
// sent, including a public IP, so the guard upstream is what rejects it.
func TestParseOffLANAddressSurfaced(t *testing.T) {
	pub := netip.MustParseAddr("8.8.8.8")
	pkt := buildAnnouncement(t,
		"evil._shelly._tcp.local.", "evil.local.", pub, 80, []string{"gen=2"})
	entries := parseEntries(pkt, shellyService)
	if len(entries) != 1 || len(entries[0].Addrs) != 1 || entries[0].Addrs[0] != pub {
		t.Fatalf("want the public addr surfaced for the guard to reject, got %+v", entries)
	}
}

// TestParseGarbageNoPanic: hostile / truncated bytes must never panic and
// must yield no entries.
func TestParseGarbageNoPanic(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x00},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		make([]byte, 12), // header only, all zero
		[]byte("not a dns message at all, just text"),
	}
	// A valid packet truncated at every length.
	full := buildAnnouncement(t, "x._shelly._tcp.local.", "x.local.",
		netip.MustParseAddr("10.0.0.5"), 80, []string{"gen=2"})
	for i := 0; i < len(full); i++ {
		cases = append(cases, full[:i])
	}
	for i, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panicked: %v", i, r)
				}
			}()
			_ = parseEntries(c, shellyService)
		}()
	}
}

func TestBuildQueryRoundTrip(t *testing.T) {
	for _, unicast := range []bool{false, true} {
		raw, err := buildQuery(shellyService, unicast)
		if err != nil {
			t.Fatalf("buildQuery(unicast=%v): %v", unicast, err)
		}
		var p dnsmessage.Parser
		if _, err := p.Start(raw); err != nil {
			t.Fatalf("parse own query: %v", err)
		}
		q, err := p.Question()
		if err != nil {
			t.Fatalf("question: %v", err)
		}
		if q.Type != dnsmessage.TypePTR {
			t.Fatalf("qtype = %v, want PTR", q.Type)
		}
		wantClass := uint16(dnsmessage.ClassINET)
		if unicast {
			wantClass |= qClassUnicast
		}
		if uint16(q.Class) != wantClass {
			t.Fatalf("qclass = %#x, want %#x", uint16(q.Class), wantClass)
		}
		if q.Name.String() != fqdn(shellyService) {
			t.Fatalf("qname = %q", q.Name.String())
		}
	}
}

func TestParseTXTFirstWins(t *testing.T) {
	m := parseTXT([]string{"gen=2", "Gen=9", "bare", "=novalue", "app=Plus1PM"})
	if m["gen"] != "2" {
		t.Fatalf("gen = %q, want first-wins 2", m["gen"])
	}
	if _, ok := m["bare"]; !ok || m["bare"] != "" {
		t.Fatalf("bare key missing/nonempty: %v", m)
	}
	if _, ok := m[""]; ok {
		t.Fatalf("empty key must be dropped: %v", m)
	}
}
