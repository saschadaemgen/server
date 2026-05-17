// Package crypto provides the HMAC-SHA256 JWT signing the mock
// daemon needs for the UDM WebSocket notification channel.
//
// Implementation is pure stdlib (crypto/hmac, crypto/sha256,
// encoding/base64, encoding/json). No third-party JWT library.
package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"carvilon.local/shared/proto"
)

// JWTHMACSecret is the HS256 secret extracted from the UDM service
// in saison 8 via heap inspection. The value is the literal UUID
// string bytes, not a binary derivation.
const JWTHMACSecret = "fce4a199-45e7-4265-9d3b-d815604921e4"

// jwtHeaderJSON is the canonical header for HS256+JWT. Field order
// in the marshalled output is fixed by struct field order so the
// serialized form is byte-stable.
type jwtHeaderJSON struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// JWTClaims is the minimal payload UDM requires for the WebSocket
// notification channel.
type JWTClaims struct {
	Sub string `json:"sub"`
	Iss string `json:"iss"`
	Exp int64  `json:"exp"`
}

// SignJWT produces an HS256-signed JWT for the given device ID.
// Lifetime is proto.JWTLifetime days from now.
func SignJWT(deviceID string) (string, error) {
	if deviceID == "" {
		return "", errors.New("crypto: deviceID must not be empty")
	}
	claims := JWTClaims{
		Sub: deviceID,
		Iss: proto.JWTIssuer,
		Exp: time.Now().Add(proto.JWTLifetime * 24 * time.Hour).Unix(),
	}
	return signWithClaims(claims, []byte(JWTHMACSecret))
}

// SignJWTWithSecret allows tests and edge cases to override the
// HMAC secret. Production code should always call SignJWT.
func SignJWTWithSecret(claims JWTClaims, secret []byte) (string, error) {
	return signWithClaims(claims, secret)
}

// VerifyJWT validates the signature and decodes the claims using
// the package secret. Does not enforce expiration.
func VerifyJWT(token string) (*JWTClaims, error) {
	return verifyWithSecret(token, []byte(JWTHMACSecret))
}

// VerifyJWTWithSecret allows tests to verify against a specific
// secret.
func VerifyJWTWithSecret(token string, secret []byte) (*JWTClaims, error) {
	return verifyWithSecret(token, secret)
}

func signWithClaims(claims JWTClaims, secret []byte) (string, error) {
	headerBytes, err := json.Marshal(jwtHeaderJSON{Alg: proto.JWTAlgorithm, Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("crypto: marshal header: %w", err)
	}
	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("crypto: marshal claims: %w", err)
	}
	headerB64 := base64.RawURLEncoding.EncodeToString(headerBytes)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadBytes)
	signingInput := headerB64 + "." + payloadB64

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	sigB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return signingInput + "." + sigB64, nil
}

func verifyWithSecret(token string, secret []byte) (*JWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("crypto: token must have 3 parts, got %d", len(parts))
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, errors.New("crypto: token has empty part")
	}
	signingInput := parts[0] + "." + parts[1]

	expected := hmac.New(sha256.New, secret)
	expected.Write([]byte(signingInput))
	wantSig := expected.Sum(nil)

	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("crypto: decode signature: %w", err)
	}
	if !hmac.Equal(gotSig, wantSig) {
		return nil, errors.New("crypto: signature mismatch")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("crypto: decode payload: %w", err)
	}
	var claims JWTClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("crypto: unmarshal claims: %w", err)
	}
	return &claims, nil
}
