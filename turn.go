package stream

import (
	"errors"
	"fmt"
	"time"

	"github.com/pion/turn/v4"
	"github.com/pion/webrtc/v4"
)

// DefaultTURNCredentialTTL is the lifetime of a minted TURN REST
// credential when the caller passes ttl <= 0. Generous enough for one ICE
// handshake; the credential only has to survive connection setup, not the
// whole session.
const DefaultTURNCredentialTTL = 5 * time.Minute

// TURNCredentials mints a short-lived TURN REST credential pair
// (username, password) from the shared secret, valid for ttl. It is a thin
// wrapper over pion's GenerateLongTermTURNRESTCredentials, so the cloud
// (and, via the side-channel, the edge) issue RFC-TURN-REST ephemeral
// credentials that the in-process relay's LongTermTURNRESTAuthHandler
// accepts with the SAME secret. The secret is never logged.
//
// The returned username is "<expiry-unix>:<user>" and the password is the
// HMAC over it; both go verbatim into a webrtc.ICEServer.
func TURNCredentials(sharedSecret []byte, user string, ttl time.Duration) (username, password string, err error) {
	if len(sharedSecret) == 0 {
		return "", "", errors.New("stream: TURN shared secret is empty")
	}
	if ttl <= 0 {
		ttl = DefaultTURNCredentialTTL
	}
	return turn.GenerateLongTermTURNRESTCredentials(string(sharedSecret), user, ttl)
}

// TURNICEServers builds the webrtc.ICEServer list advertising the
// in-process relay at publicIP. It returns TWO entries:
//
//  1. a credential-LESS STUN entry (stun:<publicIP>:<udpPort>), and
//  2. the TURN entry (turn:<publicIP>:<udpPort>?transport=udp) with the
//     given ephemeral credentials.
//
// Both are served by the SAME in-process pion relay on the same UDP port:
// a pion TURN server answers STUN Binding requests automatically and
// unauthenticated (pion/turn internal/server handleBindingRequest), so
// STUN costs nothing extra - no second server, no second port, no extra
// firewall rule.
//
// STUN is a SEPARATE, credential-less entry (not a stun: URL bolted onto
// the TURN entry's creds): ICE prefers the direct srflx path STUN
// discovers and only falls back to the TURN relay when that fails, and
// pion only accepts credentials on turn:/turns: URLs (a STUN entry must be
// credential-less). v1 advertises the UDP legs only; the TLS leg
// (turns:...:tlsPort) is a documented follow-up.
func TURNICEServers(publicIP string, udpPort int, username, password string) []webrtc.ICEServer {
	return []webrtc.ICEServer{
		{URLs: []string{fmt.Sprintf("stun:%s:%d", publicIP, udpPort)}},
		{
			URLs:       []string{fmt.Sprintf("turn:%s:%d?transport=udp", publicIP, udpPort)},
			Username:   username,
			Credential: password,
		},
	}
}
