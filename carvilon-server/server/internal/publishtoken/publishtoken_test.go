package publishtoken

import (
	"strings"
	"testing"
	"time"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestIssue_NonEmptyAndDistinct(t *testing.T) {
	iss := NewIssuer(testKey(), time.Minute)
	a, err := iss.Issue("0c:ea:14:00:00:01")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if a == "" {
		t.Fatal("Issue returned empty token")
	}
	b, err := iss.Issue("0c:ea:14:00:00:01")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if a == b {
		t.Error("two issues for the same stream produced identical tokens (nonce missing?)")
	}
}

func TestValidate_Roundtrip(t *testing.T) {
	iss := NewIssuer(testKey(), time.Minute)
	const sid = "0c:ea:14:ab:cd:ef"
	tok, err := iss.Issue(sid)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := iss.Validate(tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got != sid {
		t.Errorf("Validate stream_id = %q, want %q", got, sid)
	}
}

func TestValidate_TamperedSignature(t *testing.T) {
	iss := NewIssuer(testKey(), time.Minute)
	tok, _ := iss.Issue("s")
	// Flip the last character of the signature.
	enc, sig, _ := strings.Cut(tok, ".")
	bad := []byte(sig)
	bad[len(bad)-1] ^= 0x01
	if _, err := iss.Validate(enc + "." + string(bad)); err == nil {
		t.Fatal("Validate accepted a tampered signature")
	}
}

func TestValidate_WrongKey(t *testing.T) {
	tok, _ := NewIssuer(testKey(), time.Minute).Issue("s")
	other := make([]byte, 32) // all zeros, different key
	if _, err := NewIssuer(other, time.Minute).Validate(tok); err != ErrSignature {
		t.Errorf("Validate with wrong key err = %v, want ErrSignature", err)
	}
}

func TestValidate_Expired(t *testing.T) {
	iss := NewIssuer(testKey(), time.Minute)
	base := time.Now()
	iss.now = func() time.Time { return base }
	tok, _ := iss.Issue("s")
	// Jump past expiry.
	iss.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, err := iss.Validate(tok); err != ErrExpired {
		t.Errorf("Validate of expired token err = %v, want ErrExpired", err)
	}
}

func TestValidate_Malformed(t *testing.T) {
	iss := NewIssuer(testKey(), time.Minute)
	for _, tok := range []string{"", "nodot", ".", "payload.", ".sig"} {
		if _, err := iss.Validate(tok); err == nil {
			t.Errorf("Validate(%q) = nil error, want malformed/signature error", tok)
		}
	}
}
