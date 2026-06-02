package stream

import (
	"net"
	"sync"
	"time"

	"github.com/pion/turn/v4"

	"carvilon.local/stream/internal/icedebug"
)

// TURN telemetry (S3): the read-only data source for the carvilon admin's
// ICE/STUN/TURN menu. Two surfaces, both fed by the in-process pion relay:
//
//   - TURNStats - a point-in-time snapshot the master polls (analogous to
//     /stream/stats): allocation count + the live client set.
//   - TURNEvent - a lifecycle/auth event the master persists as history
//     (wired via CloudSetupOptions.OnTURNEvent -> SQLite).
//
// OPEN-CORE rule (same as streampublish.ICEServer at the TURN naht): these
// types carry ONLY stdlib values - strings, ints, time, bool. No pion type
// and no net.Addr crosses the seam, so the embedding module's public build
// stays pion-free. pion's net.Addr is read ONLY inside addrPair, at the
// boundary, and rendered to plain strings.
//
// SECRET rule: an event carries the ephemeral REST username (which is
// public - it travels in the SDP) but NEVER the TURN shared secret and
// NEVER the credential password. The mieter IP is carried twice - raw and
// masked - so the master chooses which to store (Sascha's decision).

// TURNStats is a point-in-time snapshot of the in-process TURN relay for
// the carvilon admin to poll. When TURN is soft-gated off (no public IP)
// Enabled is false and the other fields are zero.
type TURNStats struct {
	Enabled         bool
	AllocationCount int          // live count from turn.Server.AllocationCount()
	Clients         []TURNClient // EventHandler-maintained set of active clients
}

// TURNClient is one active relay client (one allocation). The address is
// carried raw AND masked; no secret/password appears.
type TURNClient struct {
	SrcAddr       string    // raw client address, e.g. "203.0.113.5:54321"
	SrcAddrMasked string    // coarse, non-reversible, e.g. "v4:203.0.x.x#ab12"
	Username      string    // ephemeral REST username (NOT a secret)
	Since         time.Time // when the allocation was created
}

// TURNEvent is one TURN lifecycle/auth event for the admin to persist as
// history. The mieter IP is carried raw AND masked; the TURN shared secret
// and the credential password NEVER appear.
type TURNEvent struct {
	// Kind is one of "allocation_created", "allocation_deleted",
	// "allocation_error", "auth".
	Kind string
	Time time.Time

	// SrcAddr/SrcAddrMasked: the client (mieter) address, raw and masked.
	SrcAddr       string
	SrcAddrMasked string

	// DstAddr/DstAddrMasked: the RELAYED address pion allocated for the
	// client, raw and masked. Set only on "allocation_created" (pion
	// supplies a relay address only there); "" otherwise.
	DstAddr       string
	DstAddrMasked string

	// Protocol is the client<->relay transport pion reports: "udp" or
	// "tcp" (the turns: TLS leg rides tcp).
	Protocol string

	// Username is the ephemeral REST username (NOT a secret). Empty for
	// "allocation_error" (pion supplies none there).
	Username string
	Realm    string

	// AuthOK is the auth verdict, set ONLY for Kind=="auth"; nil otherwise.
	AuthOK *bool

	// Err is a short message, set ONLY for "allocation_error" and never a
	// secret (pion's readloop error text). Empty otherwise.
	Err string
}

// turnClientSet is the EventHandler-maintained live set behind
// TURNStats.Clients. It is mutex-guarded because pion fires the lifecycle
// callbacks from its own goroutines while the master polls TURNStats
// concurrently.
type turnClientSet struct {
	mu sync.Mutex
	m  map[string]TURNClient // keyed by raw SrcAddr (one allocation per five-tuple)
}

func newTURNClientSet() *turnClientSet {
	return &turnClientSet{m: make(map[string]TURNClient)}
}

func (s *turnClientSet) add(c TURNClient) {
	s.mu.Lock()
	s.m[c.SrcAddr] = c
	s.mu.Unlock()
}

func (s *turnClientSet) remove(srcAddr string) {
	s.mu.Lock()
	delete(s.m, srcAddr)
	s.mu.Unlock()
}

func (s *turnClientSet) snapshot() []TURNClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TURNClient, 0, len(s.m))
	for _, c := range s.m {
		out = append(out, c)
	}
	return out
}

// newTURNEventHandler builds the pion turn.EventHandler that (a) maintains
// the live client set behind TURNStats and (b) forwards each lifecycle/auth
// event to onEvent (when non-nil). It converts pion's net.Addr to raw +
// masked strings AT THE BOUNDARY (addrPair), so no pion/net type ever
// escapes into the open-core TURNEvent/TURNStats. onEvent may be invoked
// concurrently from pion's goroutines; the caller must treat it as such.
func newTURNEventHandler(set *turnClientSet, onEvent func(TURNEvent)) turn.EventHandler {
	emit := func(e TURNEvent) {
		if onEvent != nil {
			onEvent(e)
		}
	}
	return turn.EventHandler{
		OnAllocationCreated: func(srcAddr, _ net.Addr, protocol, username, realm string, relayAddr net.Addr, _ int) {
			src, srcM := addrPair(srcAddr)
			dst, dstM := addrPair(relayAddr)
			now := time.Now()
			set.add(TURNClient{SrcAddr: src, SrcAddrMasked: srcM, Username: username, Since: now})
			emit(TURNEvent{
				Kind: "allocation_created", Time: now,
				SrcAddr: src, SrcAddrMasked: srcM,
				DstAddr: dst, DstAddrMasked: dstM,
				Protocol: protocol, Username: username, Realm: realm,
			})
		},
		OnAllocationDeleted: func(srcAddr, _ net.Addr, protocol, username, realm string) {
			src, srcM := addrPair(srcAddr)
			set.remove(src)
			emit(TURNEvent{
				Kind: "allocation_deleted", Time: time.Now(),
				SrcAddr: src, SrcAddrMasked: srcM,
				Protocol: protocol, Username: username, Realm: realm,
			})
		},
		OnAllocationError: func(srcAddr, _ net.Addr, protocol, message string) {
			src, srcM := addrPair(srcAddr)
			emit(TURNEvent{
				Kind: "allocation_error", Time: time.Now(),
				SrcAddr: src, SrcAddrMasked: srcM,
				Protocol: protocol, Err: message,
			})
		},
		OnAuth: func(srcAddr, _ net.Addr, protocol, username, realm, _ string, verdict bool) {
			src, srcM := addrPair(srcAddr)
			ok := verdict
			emit(TURNEvent{
				Kind: "auth", Time: time.Now(),
				SrcAddr: src, SrcAddrMasked: srcM,
				Protocol: protocol, Username: username, Realm: realm,
				AuthOK: &ok,
			})
		},
	}
}

// addrPair renders a net.Addr as (raw, masked). A nil addr yields ("", "").
// This is the ONLY place a pion-supplied net.Addr is read; the result is
// pure strings. Masking reuses the icedebug masker so all IP masking in the
// repo shares one implementation.
func addrPair(a net.Addr) (raw, masked string) {
	if a == nil {
		return "", ""
	}
	raw = a.String()
	return raw, icedebug.MaskAddr(raw)
}
