// Package egresstoken issues short-lived, streamID-bound WHEP egress
// tokens (Saison 18-14). A remote subscriber presents one to the
// stream-cloud's WHEP egress, which verifies it before fanning out the
// stream.
//
// The token is BYTE-IDENTICAL to a publish token: the same JWS-like
// payload.signature form, HMAC-SHA256 over the raw base64url payload
// string, claims {sid, exp, nonce}. The ONLY difference is the key:
// egress tokens are signed with CARVILON_EGRESS_TOKEN_HMAC_KEY, publish
// tokens with CARVILON_PUBLISH_TOKEN_HMAC_KEY. That separate key IS the
// domain separation - a publish token can never verify as an egress
// token and vice versa, with no typ/aud claim needed.
//
// We delegate to internal/publishtoken instead of cloning it, so the
// interop-critical signing bytes (the HMAC-over-raw-base64url that must
// match the stream-cloud verifier exactly) have a single source of
// truth and cannot drift.
package egresstoken

import (
	"time"

	"carvilon.local/server/internal/publishtoken"
)

// TTL is the egress-token lifetime. It matches the publish-token TTL
// and the TURN short-credential window (Sascha decision: 5 minutes).
const TTL = 5 * time.Minute

// Issuer is an alias for publishtoken.Issuer: an egress token IS a
// publish-token-format token under a different key, so it reuses the
// same Issue/Validate machinery.
type Issuer = publishtoken.Issuer

// NewIssuer returns an Issuer keyed with the egress HMAC key and the
// fixed egress TTL. key must be the hex-decoded 32-byte
// CARVILON_EGRESS_TOKEN_HMAC_KEY (see config.DecodeEgressTokenHMACKey).
func NewIssuer(key []byte) *Issuer {
	return publishtoken.NewIssuer(key, TTL)
}
