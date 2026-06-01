package stream

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v4"
)

func discardLeveledLogger() logging.LeveledLogger {
	lf := logging.NewDefaultLoggerFactory()
	lf.Writer = io.Discard
	return lf.NewLogger("test")
}

// TestTURNCredentials_RoundTrip proves a credential we mint is accepted by
// the same auth handler the in-process relay uses (same shared secret).
func TestTURNCredentials_RoundTrip(t *testing.T) {
	secret := []byte("test-only-shared-secret") // not a real secret
	user, pass, err := TURNCredentials(secret, "carvilon", time.Minute)
	if err != nil {
		t.Fatalf("TURNCredentials: %v", err)
	}
	if user == "" || pass == "" {
		t.Fatal("minted empty credential")
	}

	handler := turn.LongTermTURNRESTAuthHandler(string(secret), discardLeveledLogger())
	key, ok := handler(user, "carvilon", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1})
	if !ok {
		t.Fatal("auth handler rejected a freshly minted credential")
	}
	if len(key) == 0 {
		t.Fatal("auth handler returned an empty key")
	}
}

func TestTURNCredentials_EmptySecret(t *testing.T) {
	if _, _, err := TURNCredentials(nil, "x", time.Minute); err == nil {
		t.Fatal("expected error for empty shared secret")
	}
}

func TestTURNICEServers_Shape(t *testing.T) {
	srv := TURNICEServers("203.0.113.7", 3478, "user", "pass")
	if len(srv) != 2 {
		t.Fatalf("want 2 ICE servers (stun + turn), got %d", len(srv))
	}

	// [0] STUN: credential-less (pion only accepts creds on turn:/turns:).
	if len(srv[0].URLs) != 1 || srv[0].URLs[0] != "stun:203.0.113.7:3478" {
		t.Errorf("unexpected STUN URLs: %v", srv[0].URLs)
	}
	if srv[0].Username != "" || srv[0].Credential != nil {
		t.Errorf("STUN entry must be credential-less, got %+v", srv[0])
	}

	// [1] TURN: with the ephemeral REST creds.
	if len(srv[1].URLs) != 1 || srv[1].URLs[0] != "turn:203.0.113.7:3478?transport=udp" {
		t.Errorf("unexpected TURN URLs: %v", srv[1].URLs)
	}
	if srv[1].Username != "user" || srv[1].Credential != "pass" {
		t.Errorf("TURN creds not set: %+v", srv[1])
	}
}
