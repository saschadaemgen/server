// Package icedebug provides opt-in, IP-masking ICE diagnostics for the
// WebRTC paths (WHIP ingress, WHEP egress, RPi whipclient).
//
// It is SILENT unless CARVILON_ICE_DEBUG is set (1/true/yes/on), and it
// never logs a full IP address - only the candidate type, protocol,
// address family, and a masked address tag (first two octets for v4,
// first hextet for v6, plus a short non-reversible hash so two masked
// addresses can be told apart). The goal is to see which ICE candidate
// TYPES are gathered (host / srflx / relay), not to record addresses.
//
// Added in Stream season 3 (ICE befund) to diagnose a checking->failed
// ICE handshake before deciding STUN vs TURN. Purely additive: attaching
// it does not change ICE behaviour, and with the flag off the callbacks
// are not even registered.
package icedebug

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// Logger is the minimal logging surface icedebug needs. *log.Logger
// satisfies it, as does any logger exposing a Printf method.
type Logger interface {
	Printf(format string, v ...any)
}

// enabled reads CARVILON_ICE_DEBUG once. The env is fixed at process
// start, so a single read is correct and cheap.
var enabled = sync.OnceValue(func() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CARVILON_ICE_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
})

// Enabled reports whether ICE debug logging is on (env CARVILON_ICE_DEBUG).
func Enabled() bool { return enabled() }

// AttachCandidateLogging logs every gathered ICE candidate for pc - type,
// protocol, address family, masked address - but ONLY when
// CARVILON_ICE_DEBUG is set. With the flag off it is a no-op and does not
// register the callback. The WebRTC paths here do not otherwise use
// OnICECandidate (non-trickle: gather-then-answer), so nothing is
// clobbered. label identifies the connection in the log line.
func AttachCandidateLogging(pc *webrtc.PeerConnection, logger Logger, label string) {
	if pc == nil || logger == nil || !Enabled() {
		return
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			logger.Printf("ICE-DEBUG %s: candidate gathering complete", label)
			return
		}
		logger.Printf("ICE-DEBUG %s: candidate typ=%s proto=%s addr=%s",
			label, c.Typ.String(), c.Protocol.String(), maskAddr(c.Address))
	})
}

// StateTracker logs ICE connection-state transitions with elapsed time
// since creation, ONLY when CARVILON_ICE_DEBUG is set. One per
// PeerConnection. Construct it just before the OnICEConnectionStateChange
// handler and call Log from inside it; this AUGMENTS (does not replace)
// the path's existing state handling.
type StateTracker struct {
	logger Logger
	label  string
	start  time.Time
}

// NewStateTracker returns a tracker stamped at "now". Cheap; safe to
// create even when the flag is off (Log is then a no-op).
func NewStateTracker(logger Logger, label string) *StateTracker {
	return &StateTracker{logger: logger, label: label, start: time.Now()}
}

// Log records one ICE state transition with t+<elapsed>s, gated on the
// flag. No-op on a nil tracker or with the flag off.
func (s *StateTracker) Log(state webrtc.ICEConnectionState) {
	if s == nil || s.logger == nil || !Enabled() {
		return
	}
	s.logger.Printf("ICE-DEBUG %s: state=%s t+%.1fs", s.label, state.String(), time.Since(s.start).Seconds())
}

// MaskAddr is the exported masker reused by the TURN telemetry layer
// (package stream) so all IP masking in the repo shares one implementation.
// It accepts a bare IP or a host:port (e.g. "203.0.113.5:54321"), strips
// the port, and masks the host exactly like the candidate logger. It NEVER
// returns the full address. An empty input yields "".
func MaskAddr(addr string) string {
	if addr == "" {
		return ""
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	return maskAddr(host)
}

// maskAddr returns a low-cardinality, non-reversible tag for an ICE
// candidate address: family + a coarse prefix + a short hash. It NEVER
// returns the full address, so it is safe to log. A non-IP address (e.g.
// an mDNS .local name) yields only the hash.
func maskAddr(addr string) string {
	h := sha256.Sum256([]byte(addr))
	tag := hex.EncodeToString(h[:2]) // 4 hex chars - distinctness only
	ip := net.ParseIP(addr)
	switch {
	case ip == nil:
		return "non-ip#" + tag
	case ip.To4() != nil:
		v4 := ip.To4()
		return fmt.Sprintf("v4:%d.%d.x.x#%s", v4[0], v4[1], tag)
	default:
		return fmt.Sprintf("v6:%02x%02x::#%s", ip[0], ip[1], tag)
	}
}
