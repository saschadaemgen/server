package publishtoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// makeToken signiert einen echten Token fuer Tests: base64url(payload-
// JSON) + "." + base64url(HMAC-SHA256(payload-bytes, key)). Spiegelt
// exakt das Format, das Verify erwartet — wir testen also gegen echte
// Tokens, nicht gegen handgeschnitzte Mock-Strings. Nicht exportiert:
// lebt nur im Test-Paket.
func makeToken(t *testing.T, p Payload, key []byte) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	payloadPart := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payloadPart))
	sigPart := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payloadPart + "." + sigPart
}

// tamperPayload ersetzt das payload-Segment durch eine frisch kodierte,
// abweichende payload, BEHAELT aber die Original-Signatur. Modelliert
// einen Angreifer, der Claims editiert ohne den HMAC-Key zu kennen —
// die HMAC ueber die neue payload passt dann nicht mehr.
func tamperPayload(t *testing.T, token string, newPayload Payload) string {
	t.Helper()
	_, sigPart, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token has no dot: %q", token)
	}
	raw, err := json.Marshal(newPayload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	newPayloadPart := base64.RawURLEncoding.EncodeToString(raw)
	return newPayloadPart + "." + sigPart
}

func TestVerify(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	const sid = "aa:bb:cc:dd:ee:ff"
	// Fester now-Wert macht die Tabelle deterministisch.
	now := time.Unix(1_700_000_000, 0).UTC()

	valid := makeToken(t, Payload{SID: sid, Exp: now.Unix() + 60, Nonce: "n1"}, key)

	tests := []struct {
		name    string
		token   string
		sid     string
		wantErr error
	}{
		{
			name:    "happy path",
			token:   valid,
			sid:     sid,
			wantErr: nil,
		},
		{
			name:    "malformed - empty token",
			token:   "",
			sid:     sid,
			wantErr: ErrMalformed,
		},
		{
			name:    "malformed - no dot",
			token:   "justonesegment",
			sid:     sid,
			wantErr: ErrMalformed,
		},
		{
			name:    "malformed - three parts",
			token:   "a.b.c",
			sid:     sid,
			wantErr: ErrMalformed,
		},
		{
			name:    "malformed - empty payload segment",
			token:   "." + strings.SplitN(valid, ".", 2)[1],
			sid:     sid,
			wantErr: ErrMalformed,
		},
		{
			name:    "malformed - empty signature segment",
			token:   strings.SplitN(valid, ".", 2)[0] + ".",
			sid:     sid,
			wantErr: ErrMalformed,
		},
		{
			// payload geaendert, Original-Signatur behalten -> HMAC passt nicht.
			name:    "wrong signature - tampered payload",
			token:   tamperPayload(t, valid, Payload{SID: sid, Exp: now.Unix() + 60, Nonce: "evil"}),
			sid:     sid,
			wantErr: ErrSignature,
		},
		{
			// korrekt signierter Token, aber auf eine andere sid ausgestellt.
			name:    "sid mismatch",
			token:   makeToken(t, Payload{SID: "other-sid", Exp: now.Unix() + 60, Nonce: "n"}, key),
			sid:     sid,
			wantErr: ErrSIDMismatch,
		},
		{
			name:    "expired - exp in past",
			token:   makeToken(t, Payload{SID: sid, Exp: now.Unix() - 1, Nonce: "n"}, key),
			sid:     sid,
			wantErr: ErrExpired,
		},
		{
			// Grenzfall: exp == now. Per Spec strikt (<), also abgelaufen.
			name:    "expired - boundary exp == now",
			token:   makeToken(t, Payload{SID: sid, Exp: now.Unix(), Nonce: "n"}, key),
			sid:     sid,
			wantErr: ErrExpired,
		},
		{
			// Eine Sekunde Restlaufzeit -> noch gueltig (komplementaer
			// zum Grenzfall: beweist, dass < und nicht <= gilt).
			name:    "valid - one second left",
			token:   makeToken(t, Payload{SID: sid, Exp: now.Unix() + 1, Nonce: "n"}, key),
			sid:     sid,
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.token, tc.sid, key, now)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Verify() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestVerify_WrongKeyFails guards the core security property: a token
// signed with a different key must never verify, even when every claim
// (sid, exp) is otherwise valid.
func TestVerify_WrongKeyFails(t *testing.T) {
	const sid = "cam-1"
	now := time.Unix(1_700_000_000, 0).UTC()
	signingKey := []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	verifyKey := []byte("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")

	tok := makeToken(t, Payload{SID: sid, Exp: now.Unix() + 60, Nonce: "n"}, signingKey)
	if err := Verify(tok, sid, verifyKey, now); !errors.Is(err, ErrSignature) {
		t.Errorf("Verify with wrong key = %v, want ErrSignature", err)
	}
}
