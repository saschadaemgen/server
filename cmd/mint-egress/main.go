// Command mint-egress mints a short-lived token for manually testing the
// WHEP egress auth against a running stream server. The token is
// byte-identical to a CARVILON publish/egress token:
//
//	base64url(json{sid,exp,nonce}) + "." + base64url(HMAC-SHA256(key, payloadString))
//
// where the HMAC is taken over the base64url payload STRING - exactly what
// internal/publishtoken.Verify expects - so the server accepts the token when
// it is signed with the matching key.
//
// It is a DIAGNOSTIC HELPER, not part of the production build: it is its own
// package main and the edge/cloud binaries never import it. The HMAC key is
// read ONLY from an environment variable (-key-env, default
// CARVILON_EGRESS_TOKEN_HMAC_KEY) - never a flag (a key on the command line
// would land in the shell history), never hard-coded, never logged - not even
// on failure. stdout carries ONLY the token, so `TOKEN=$(go run ...)` is
// clean; all diagnostics go to stderr.
//
// Usage:
//
//	# valid egress token (CARVILON_EGRESS_TOKEN_HMAC_KEY set in the env):
//	go run ./cmd/mint-egress -sid <streamID>
//	go run ./cmd/mint-egress -sid cam-1 -ttl 10m
//
//	# publish-key token, for the key-separation 401 test (must be rejected):
//	go run ./cmd/mint-egress -sid <streamID> -key-env CARVILON_PUBLISH_TOKEN_HMAC_KEY
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	sid := flag.String("sid", "", "streamID the token is bound to (must match the /whep/<sid> URL) (required)")
	ttl := flag.Duration("ttl", 5*time.Minute, "token lifetime")
	keyEnv := flag.String("key-env", "CARVILON_EGRESS_TOKEN_HMAC_KEY", "name of the env var holding the 32-byte hex HMAC key")
	flag.Parse()

	if *sid == "" {
		fmt.Fprintln(os.Stderr, "mint-egress: -sid is required")
		os.Exit(2)
	}

	// Key ONLY from the environment. Never echo the key (or a hex attempt),
	// not even on a decode failure.
	keyHex := os.Getenv(*keyEnv)
	if keyHex == "" {
		fmt.Fprintf(os.Stderr, "mint-egress: env %s is not set (the HMAC key must come from the environment)\n", *keyEnv)
		os.Exit(2)
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mint-egress: env %s is not valid hex\n", *keyEnv)
		os.Exit(2)
	}
	if len(key) != 32 {
		fmt.Fprintf(os.Stderr, "mint-egress: env %s must be 32 bytes hex-encoded (got %d bytes)\n", *keyEnv, len(key))
		os.Exit(2)
	}

	nonce, err := randomNonce()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mint-egress: nonce: %v\n", err)
		os.Exit(1)
	}

	// Payload: the same shape as internal/publishtoken.Payload (sid/exp/nonce).
	payload, err := json.Marshal(struct {
		SID   string `json:"sid"`
		Exp   int64  `json:"exp"`
		Nonce string `json:"nonce"`
	}{
		SID:   *sid,
		Exp:   time.Now().Add(*ttl).Unix(),
		Nonce: nonce,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mint-egress: marshal payload: %v\n", err)
		os.Exit(1)
	}

	// HMAC over the base64url payload STRING (matches publishtoken.Verify).
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(p))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	fmt.Println(p + "." + sig) // ONLY the token on stdout
}

// randomNonce returns 9 random bytes as base64url-no-pad, so two mints never
// collide on the nonce.
func randomNonce() (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
