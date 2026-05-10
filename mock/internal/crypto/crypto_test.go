package crypto

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const testDeviceID = "0cea14424242"

func TestSignJWT_RoundTrip(t *testing.T) {
	token, err := SignJWT(testDeviceID)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	claims, err := VerifyJWT(token)
	if err != nil {
		t.Fatalf("VerifyJWT: %v", err)
	}
	if claims.Sub != testDeviceID {
		t.Errorf("Sub = %q, want %q", claims.Sub, testDeviceID)
	}
	if claims.Iss != "unifi-access" {
		t.Errorf("Iss = %q, want %q", claims.Iss, "unifi-access")
	}
	now := time.Now().Unix()
	if claims.Exp <= now {
		t.Errorf("Exp = %d, want > now (%d)", claims.Exp, now)
	}
	upper := time.Now().Add(16 * 24 * time.Hour).Unix()
	if claims.Exp >= upper {
		t.Errorf("Exp = %d, want < now+16d (%d)", claims.Exp, upper)
	}
}

func TestSignJWT_EmptyDeviceID(t *testing.T) {
	if _, err := SignJWT(""); err == nil {
		t.Fatal("SignJWT(\"\") accepted; want error")
	}
}

func TestSignJWT_StructuralCheck(t *testing.T) {
	token, err := SignJWT(testDeviceID)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr map[string]string
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if hdr["alg"] != "HS256" {
		t.Errorf("header.alg = %q, want HS256", hdr["alg"])
	}
	if hdr["typ"] != "JWT" {
		t.Errorf("header.typ = %q, want JWT", hdr["typ"])
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := payload["sub"]; !ok {
		t.Error("payload missing sub")
	}
	if _, ok := payload["iss"]; !ok {
		t.Error("payload missing iss")
	}
	if _, ok := payload["exp"]; !ok {
		t.Error("payload missing exp")
	}
}

func TestVerifyJWT_TamperedPayload(t *testing.T) {
	token, err := SignJWT(testDeviceID)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("empty payload")
	}
	payload[0] ^= 0x01
	parts[1] = base64.RawURLEncoding.EncodeToString(payload)
	tampered := strings.Join(parts, ".")
	if _, err := VerifyJWT(tampered); err == nil {
		t.Fatal("tampered payload accepted; want error")
	}
}

func TestVerifyJWT_TamperedSignature(t *testing.T) {
	token, err := SignJWT(testDeviceID)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	parts := strings.Split(token, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if len(sig) == 0 {
		t.Fatal("empty sig")
	}
	sig[0] ^= 0x01
	parts[2] = base64.RawURLEncoding.EncodeToString(sig)
	tampered := strings.Join(parts, ".")
	if _, err := VerifyJWT(tampered); err == nil {
		t.Fatal("tampered signature accepted; want error")
	}
}

func TestVerifyJWT_WrongSecret(t *testing.T) {
	claims := JWTClaims{Sub: testDeviceID, Iss: "unifi-access", Exp: time.Now().Add(time.Hour).Unix()}
	token, err := SignJWTWithSecret(claims, []byte("secret-A"))
	if err != nil {
		t.Fatalf("SignJWTWithSecret: %v", err)
	}
	if _, err := VerifyJWTWithSecret(token, []byte("secret-B")); err == nil {
		t.Fatal("verify with wrong secret succeeded; want error")
	}
}

func TestVerifyJWT_MalformedToken(t *testing.T) {
	cases := []string{
		"not.a.jwt-but-3-parts-yet-bad-base64",
		"only.two",
		"",
		"a.b.",
		".b.c",
	}
	for _, tok := range cases {
		if _, err := VerifyJWT(tok); err == nil {
			t.Errorf("VerifyJWT(%q) accepted; want error", tok)
		}
	}
}
