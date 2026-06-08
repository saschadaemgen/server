package egresstoken

import (
	"errors"
	"testing"
	"time"

	"carvilon.local/server/internal/publishtoken"
)

// key returns a deterministic 32-byte HMAC key for tests.
func key(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	iss := NewIssuer(key(0x11))
	const sid = "0c:ea:14:42:42:99"
	tok, err := iss.Issue(sid)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := iss.Validate(tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got != sid {
		t.Fatalf("sid round-trip = %q, want %q", got, sid)
	}
}

func TestExpired(t *testing.T) {
	k := key(0x22)
	// A negative TTL puts exp in the past, so the token is born expired -
	// no clock hook needed to exercise the egress validate's expiry path.
	tok, err := publishtoken.NewIssuer(k, -time.Minute).Issue("aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := NewIssuer(k).Validate(tok); !errors.Is(err, publishtoken.ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

// TestKeySeparation is the domain-separation proof: a token signed with
// the PUBLISH key must not validate under the egress key, and vice
// versa. With no typ/aud claim, the separate key is the only thing that
// keeps the two token kinds apart.
func TestKeySeparation(t *testing.T) {
	publishKey := key(0xAA)
	egressKey := key(0xBB)
	const sid = "0c:ea:14:00:00:01"

	publishTok, err := publishtoken.NewIssuer(publishKey, time.Minute).Issue(sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewIssuer(egressKey).Validate(publishTok); !errors.Is(err, publishtoken.ErrSignature) {
		t.Fatalf("publish-key token must fail egress validate with ErrSignature, got %v", err)
	}

	egressTok, err := NewIssuer(egressKey).Issue(sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishtoken.NewIssuer(publishKey, time.Minute).Validate(egressTok); !errors.Is(err, publishtoken.ErrSignature) {
		t.Fatalf("egress token must fail publish validate with ErrSignature, got %v", err)
	}
}

func TestTTLIsFiveMinutes(t *testing.T) {
	if TTL != 5*time.Minute {
		t.Fatalf("TTL = %v, want 5m", TTL)
	}
}
