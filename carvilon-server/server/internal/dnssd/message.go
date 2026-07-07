package dnssd

import (
	"net/netip"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// qClassUnicast is the mDNS "unicast response requested" (QU) bit, ORed
// onto the query class. A responder that sees it MAY answer by unicast to
// our source port - the mechanism the ephemeral-port fallback relies on
// when it cannot bind 5353 to receive multicast (RFC 6762 section 5.4).
const qClassUnicast = 0x8000

// buildQuery packs a single-question PTR query for service (e.g.
// "_shelly._tcp.local"). When unicast is set, the QU bit is added so the
// answer can come straight back to an ephemeral socket.
func buildQuery(service string, unicast bool) ([]byte, error) {
	name, err := dnsmessage.NewName(fqdn(service))
	if err != nil {
		return nil, err
	}
	class := dnsmessage.ClassINET
	if unicast {
		class = dnsmessage.Class(qClassUnicast | uint16(dnsmessage.ClassINET))
	}
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{RecursionDesired: false},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  dnsmessage.TypePTR,
			Class: class,
		}},
	}
	return msg.Pack()
}

// srvRec is the SRV target host + port for one instance.
type srvRec struct {
	target string
	port   uint16
}

// parseEntries decodes an mDNS message (a query response or an unsolicited
// announcement) and assembles one Entry per service instance of `service`.
// It is deliberately tolerant: a malformed record is skipped, a truncated
// message yields whatever parsed cleanly, and it never panics on hostile
// input - the whole point of owning this function is that it is the trust
// boundary for untrusted LAN multicast.
//
// Matching is case-insensitive (mDNS names are), and only instances that
// belong to `service` are returned; foreign services in the same packet
// are ignored.
func parseEntries(payload []byte, service string) []Entry {
	svc := strings.ToLower(fqdn(service))

	var p dnsmessage.Parser
	if _, err := p.Start(payload); err != nil {
		return nil
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil
	}

	// Records, collected across all three resource sections. Keyed by the
	// lowercased owner name; original casing is preserved for display.
	instances := map[string]bool{} // lower instance name -> present
	orig := map[string]string{}    // lower -> original-cased name
	srv := map[string]srvRec{}     // lower instance -> SRV
	txt := map[string][]string{}   // lower instance -> raw TXT strings
	a := map[string][]netip.Addr{} // lower host -> A addrs

	remember := func(name string) string {
		low := strings.ToLower(name)
		if _, ok := orig[low]; !ok {
			orig[low] = name
		}
		return low
	}
	noteInstance := func(name string) string {
		low := remember(name)
		// Only names that belong to the watched service become instances.
		if strings.HasSuffix(low, "."+svc) {
			instances[low] = true
		}
		return low
	}

	// consume walks one section: read each header, dispatch on type, and
	// skip anything else with the section-specific skip so the parser stays
	// in lock-step with the wire.
	consume := func(header func() (dnsmessage.ResourceHeader, error), skip func() error) bool {
		for {
			h, err := header()
			if err == dnsmessage.ErrSectionDone {
				return true
			}
			if err != nil {
				return false
			}
			switch h.Type {
			case dnsmessage.TypePTR:
				r, err := p.PTRResource()
				if err != nil {
					return false
				}
				// A PTR whose owner is the service maps to an instance.
				if strings.EqualFold(h.Name.String(), svc) {
					noteInstance(r.PTR.String())
				} else {
					remember(h.Name.String())
				}
			case dnsmessage.TypeSRV:
				r, err := p.SRVResource()
				if err != nil {
					return false
				}
				low := noteInstance(h.Name.String())
				srv[low] = srvRec{target: r.Target.String(), port: r.Port}
			case dnsmessage.TypeTXT:
				r, err := p.TXTResource()
				if err != nil {
					return false
				}
				low := noteInstance(h.Name.String())
				txt[low] = append(txt[low], r.TXT...)
			case dnsmessage.TypeA:
				r, err := p.AResource()
				if err != nil {
					return false
				}
				low := remember(h.Name.String())
				if addr := netip.AddrFrom4(r.A); addr.IsValid() {
					a[low] = append(a[low], addr)
				}
			default:
				if err := skip(); err != nil {
					return false
				}
			}
		}
	}

	if !consume(p.AnswerHeader, p.SkipAnswer) {
		// Fall through: return whatever we gathered before the fault.
		return assemble(service, instances, orig, srv, txt, a)
	}
	if !consume(p.AuthorityHeader, p.SkipAuthority) {
		return assemble(service, instances, orig, srv, txt, a)
	}
	consume(p.AdditionalHeader, p.SkipAdditional)
	return assemble(service, instances, orig, srv, txt, a)
}

// assemble joins the collected records into per-instance Entries.
func assemble(service string, instances map[string]bool, orig map[string]string, srv map[string]srvRec, txt map[string][]string, a map[string][]netip.Addr) []Entry {
	var out []Entry
	for low := range instances {
		e := Entry{Instance: orig[low], Service: fqdn(service)}
		if s, ok := srv[low]; ok {
			e.Host = s.target
			e.Port = s.port
			if addrs, ok := a[strings.ToLower(s.target)]; ok {
				e.Addrs = dedupeAddrs(addrs)
			}
		}
		if raw, ok := txt[low]; ok {
			e.TXT = parseTXT(raw)
		}
		out = append(out, e)
	}
	return out
}

// parseTXT turns raw "key=value" TXT strings into a map. A bare key (no
// '=') maps to "". Keys are lowercased (DNS-SD keys are case-insensitive);
// the first occurrence of a key wins (RFC 6763 section 6.4).
func parseTXT(raw []string) map[string]string {
	m := make(map[string]string, len(raw))
	for _, s := range raw {
		if s == "" {
			continue
		}
		k, v, _ := strings.Cut(s, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}
	return m
}

// dedupeAddrs removes duplicate addresses while keeping order.
func dedupeAddrs(in []netip.Addr) []netip.Addr {
	seen := make(map[netip.Addr]bool, len(in))
	out := in[:0:0]
	for _, a := range in {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}
