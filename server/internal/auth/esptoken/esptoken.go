// Package esptoken erzeugt und verifiziert Bearer-Tokens fuer
// adoptierte ESP-Viewer. Maschine-zu-Maschine-Auth - SHA-256
// reicht, weil 32 Zufalls-Bytes aus crypto/rand genug Entropie
// haben und es kein User-Eingabe-Material gibt das offline
// brute-force-anfaellig waere. Argon2id (wie beim Viewer-PW)
// waere hier Overkill.
//
// Saison 13-02-FIX4-c fuehrt das Paket ein; FIX4-d nutzt es in
// der Bearer-Auth-Middleware fuer den /esp/-API-Tree.
package esptoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// TokenByteLen ist die Anzahl Zufalls-Bytes pro Token. 32 ergeben
// einen 43-Zeichen base64url-String, ~256 Bit Entropie.
const TokenByteLen = 32

// Generate liefert ein frisches Token-Paar:
//   - clear: base64url-encoded Zufalls-Bytes, geht in die Adopt-
//     Response an den ESP. Server haelt das NICHT.
//   - hash: SHA-256 als hex-string, wird in viewers.esp_token_hash
//     abgelegt.
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

// Verify prueft ob der vom ESP praesentierte Klartext-Token zum
// gespeicherten Hash passt. crypto/subtle.ConstantTimeCompare
// gegen Timing-Leaks.
func Verify(presented, storedHash string) bool {
	if presented == "" || storedHash == "" {
		return false
	}
	sum := sha256.Sum256([]byte(presented))
	got := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1
}

// Preview liefert die ersten 8 Zeichen des Klartext-Tokens fuer
// das Admin-Modal ("token_preview: 'abc12345...'"). Reicht zum
// optischen Pruefen ohne den vollen Token im Admin-Audit zu
// haben.
func Preview(clear string) string {
	if len(clear) <= 8 {
		return clear
	}
	return clear[:8] + "..."
}
