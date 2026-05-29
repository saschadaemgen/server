// Package publishtoken verifies short-lived JWS-like publish tokens.
//
// Token-Format (mit carvilon-edge token-issuer abgestimmt):
//
//	token     := payload + "." + signature
//	payload   := base64url(json({"sid": "...", "exp": <unix-seconds>, "nonce": "..."}))
//	signature := base64url(HMAC-SHA256(payload-bytes, hmac-key))
//
// base64url ist ohne Padding (RFC 7515 Konvention).
//
// Die Verifikation ist bewusst defensiv geordnet: zuerst die HMAC-
// Signatur (gegen das ROHE payload-Segment, in konstanter Zeit), erst
// danach wird die payload dekodiert und das JSON geparst. So erreicht
// eine manipulierte payload den JSON-Parser nie.
package publishtoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Payload ist die JSON-Struktur in der payload-Komponente.
type Payload struct {
	SID   string `json:"sid"`
	Exp   int64  `json:"exp"` // Unix-Sekunden UTC
	Nonce string `json:"nonce"`
}

// Sentinel-Fehler. Exportiert, damit Tests (und spaeter ein edge-seitiger
// Round-trip-Check) per errors.Is gegen die konkrete Klasse pruefen
// koennen.
//
// WICHTIG: Die HTTP-Schicht darf NIE eine dieser Klassen an den Client
// durchreichen — nach aussen immer nur 401 ohne Detail (Schutz gegen
// Oracle-/Timing-Aufklaerung, an welchem Check es scheiterte). Das
// konkrete Detail gehoert ausschliesslich ins server-seitige Log.
var (
	ErrMalformed   = errors.New("publishtoken: malformed token")
	ErrSignature   = errors.New("publishtoken: signature mismatch")
	ErrSIDMismatch = errors.New("publishtoken: sid mismatch")
	ErrExpired     = errors.New("publishtoken: token expired")
)

// Verify prueft Signatur, Ablauf und sid-Bindung eines Tokens.
//
// expectedSID ist die streamID aus der WHIP-URL; der Token muss exakt
// auf diese sid lauten. now wird injiziert (Test-Hook); Produktion ruft
// mit time.Now().UTC().
//
// Reihenfolge (sicherheitsrelevant):
//
//  1. Struktur: genau zwei nicht-leere Segmente, getrennt durch ".".
//  2. HMAC-SHA256 ueber das ROHE payload-Segment, konstant-Zeit-
//     Vergleich gegen die mitgelieferte Signatur.
//  3. Erst nach Signatur-OK: payload base64url-decode + JSON-parse.
//  4. sid-Bindung (payload.SID == expectedSID).
//  5. Ablauf: gueltig nur solange now strikt vor exp liegt; exp == now
//     gilt bereits als abgelaufen.
func Verify(token, expectedSID string, key []byte, now time.Time) error {
	// 1. Struktur: genau zwei nicht-leere Teile.
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ErrMalformed
	}
	payloadPart, sigPart := parts[0], parts[1]

	// 2. Signatur ueber das ROHE payload-Segment, in konstanter Zeit.
	// ConstantTimeCompare liefert 0 auch bei Laengen-Unterschied, der
	// String-Vergleich der base64url-Form ist also gefahrlos.
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payloadPart))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expectedSig), []byte(sigPart)) != 1 {
		return ErrSignature
	}

	// 3. Erst jetzt dekodieren + parsen — die Signatur ist verifiziert,
	// die payload-Bytes sind also von uns selbst erzeugt worden.
	raw, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return ErrMalformed
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return ErrMalformed
	}

	// 4. sid-Bindung.
	if p.SID != expectedSID {
		return ErrSIDMismatch
	}

	// 5. Ablauf. Strikt: now.Unix() < exp. exp == now -> abgelaufen.
	if now.Unix() >= p.Exp {
		return ErrExpired
	}
	return nil
}
