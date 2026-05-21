// Package esptoken generates and verifies bearer tokens for
// adopted ESP viewers. Machine-to-machine auth - SHA-256 is
// enough, because 32 random bytes from crypto/rand carry enough
// entropy and there is no user-supplied material that would be
// offline-brute-forceable. Argon2id (used for viewer passwords)
// would be overkill here.
//
// The bearer-auth middleware for the /esp/ API tree calls
// Verify with the token presented in the Authorization header.
package esptoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// TokenByteLen is the number of random bytes per token. 32 bytes
// produce a 43-char base64url string carrying ~256 bits of entropy.
const TokenByteLen = 32

// Generate returns a fresh token pair:
//   - clear: base64url-encoded random bytes returned to the ESP
//     in the adopt response. The server does NOT keep this.
//   - hash:  SHA-256 as a hex string, stored in
//     viewers.esp_token_hash.
func Generate() (clear string, hash string, err error) {
	buf := make([]byte, TokenByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	clear = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(clear))
	hash = hex.EncodeToString(sum[:])
	return clear, hash, nil
}

// Verify checks whether the clear-text token presented by the ESP
// matches the stored hash. Uses crypto/subtle.ConstantTimeCompare
// to avoid timing leaks.
func Verify(presented, storedHash string) bool {
	if presented == "" || storedHash == "" {
		return false
	}
	sum := sha256.Sum256([]byte(presented))
	got := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1
}

// Preview returns the first 8 characters of the clear-text token
// for the admin modal ("token_preview: 'abc12345...'"). Enough
// for a visual check without keeping the full token in the
// admin audit trail.
func Preview(clear string) string {
	if len(clear) <= 8 {
		return clear
	}
	return clear[:8] + "..."
}
