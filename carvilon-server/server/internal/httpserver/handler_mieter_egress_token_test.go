// Tests for GET /webviewer/egress-token (Saison 18-14).
package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"carvilon.local/server/internal/egresstoken"
)

// egressTestKey is a deterministic 32-byte HMAC key for the tests.
func egressTestKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0x5a
	}
	return k
}

// TestEgressToken_RequiresAuth proves the endpoint is gated: without a
// viewer session there is NO token, even when the issuer is configured.
func TestEgressToken_RequiresAuth(t *testing.T) {
	env := newTestServer(t)
	env.srv.egressIssuer = egresstoken.NewIssuer(egressTestKey()) // issuer present...
	// ...but no login, so requireViewerAuth refuses before the handler.
	resp, err := env.client.Get(env.ts.URL + "/webviewer/egress-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect to /login)", resp.StatusCode)
	}
}

// TestEgressToken_NotConfigured proves the soft gate: an authenticated
// viewer with no egress key configured gets a clean 503, not a crash.
func TestEgressToken_NotConfigured(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	env.loginViewer(t, testViewerName, testViewerPassword)
	// egressIssuer is nil (default Deps).
	resp, err := env.client.Get(env.ts.URL + "/webviewer/egress-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestEgressToken_IssuesForViewer proves the happy path: an authenticated
// viewer gets a token that validates under the egress key and binds to
// that viewer's MAC (= streamID).
func TestEgressToken_IssuesForViewer(t *testing.T) {
	env := newTestServer(t)
	key := egressTestKey()
	env.srv.egressIssuer = egresstoken.NewIssuer(key)
	env.seedViewer(t)
	env.loginViewer(t, testViewerName, testViewerPassword)

	resp, err := env.client.Get(env.ts.URL + "/webviewer/egress-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Token     string `json:"token"`
		StreamID  string `json:"stream_id"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.StreamID != testViewerMAC {
		t.Fatalf("stream_id = %q, want %q", body.StreamID, testViewerMAC)
	}
	if body.ExpiresIn != 300 {
		t.Fatalf("expires_in = %d, want 300", body.ExpiresIn)
	}
	// The token must validate under the egress key and resolve to the
	// viewer's MAC. (Stream-side verify uses the same form under the same
	// key; here we use the carvilon-side Validate as the round-trip.)
	sid, err := egresstoken.NewIssuer(key).Validate(body.Token)
	if err != nil {
		t.Fatalf("validate issued token: %v", err)
	}
	if sid != testViewerMAC {
		t.Fatalf("token sid = %q, want %q", sid, testViewerMAC)
	}
}
