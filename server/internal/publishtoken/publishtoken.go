// Package publishtoken issues and validates the short-lived tokens
// carvilon hands to the stream-edge for a WHIP push. carvilon is the
// authority: it knows the tenant, the viewer MAC and the permission,
// so it ISSUES the token; the cloud / stream-edge only presents it.
//
// This iteration signs with HMAC-SHA256 over a compact JSON claim set,
// deliberately shaped like a JWS ("payload.signature") so the later
// swap to a real (asymmetric) JWT is a single local change confined to
// Issue/Validate. The wire field (publish_token) and every caller stay
// the same. Target shape: security.md 12.7 (VPS verifies with a public
// key, signs nothing itself).
package publishtoken

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel validation errors.
var (
	ErrMalformed = errors.New("publishtoken: malformed token")
	ErrSignature = errors.New("publishtoken: bad signature")
	ErrExpired   = errors.New("publishtoken: expired")
)

// Issuer signs and verifies publish tokens with a symmetric key.
// Build one with NewIssuer and reuse it.
type Issuer struct {
	key []byte
	ttl time.Duration
	now func() time.Time // injectable clock; tests override
}

// NewIssuer builds an Issuer. key is the 32-byte HMAC key; on the edge
// it is the decoded CARVILON_PUBLISH_TOKEN_HMAC_KEY - the same key the
// stream-cloud layer verifies with, kept separate from the master key
// so the master key stays isolated on the RPi. ttl is the token
// lifetime.
func NewIssuer(key []byte, ttl time.Duration) *Issuer {
	return &Issuer{key: key, ttl: ttl, now: time.Now}
}

type claims struct {
	StreamID string `json:"sid"`
	Exp      int64  `json:"exp"`
	Nonce    string `json:"nonce"`
}

// Issue returns a signed token for streamID. Each call embeds a fresh
// random nonce, so two issues for the same stream differ.
func (i *Issuer) Issue(streamID string) (string, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("publishtoken: nonce: %w", err)
	}
	payload, err := json.Marshal(claims{
		StreamID: streamID,
		Exp:      i.now().Add(i.ttl).Unix(),
		Nonce:    base64.RawURLEncoding.EncodeToString(nonce),
	})
	if err != nil {
		return "", fmt.Errorf("publishtoken: marshal: %w", err)
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	return enc + "." + i.sign(enc), nil
}

// Validate checks the signature (constant time) and expiry, returning
// the streamID the token was issued for.
func (i *Issuer) Validate(token string) (string, error) {
	enc, sig, ok := strings.Cut(token, ".")
	if !ok || enc == "" || sig == "" {
		return "", ErrMalformed
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(i.sign(enc))) != 1 {
		return "", ErrSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", ErrMalformed
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", ErrMalformed
	}
	if i.now().Unix() > c.Exp {
		return "", ErrExpired
	}
	return c.StreamID, nil
}

func (i *Issuer) sign(enc string) string {
	m := hmac.New(sha256.New, i.key)
	m.Write([]byte(enc))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
