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
	if len(srv) != 1 {
		t.Fatalf("want 1 ICE server, got %d", len(srv))
	}
	if len(srv[0].URLs) != 1 || srv[0].URLs[0] != "turn:203.0.113.7:3478?transport=udp" {
		t.Errorf("unexpected URLs: %v", srv[0].URLs)
	}
	if srv[0].Username != "user" || srv[0].Credential != "pass" {
		t.Errorf("creds not set on ICE server: %+v", srv[0])
	}
}
