package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"carvilon.local/server/internal/console"
	"carvilon.local/server/internal/consolestore"
)

func TestConsoleProfiles_RequireAuth(t *testing.T) {
	env := newTestServer(t)
	for _, path := range []string{
		"/a/designer/console/caps",
		"/a/designer/console/profiles",
		"/a/designer/console/ws",
	} {
		resp, err := env.client.Get(env.ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("unauthenticated %s = %d, want 303", path, resp.StatusCode)
		}
	}
}

func TestConsoleCaps_ReportsLocalShellSupport(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp, err := env.client.Get(env.ts.URL + "/a/designer/console/caps")
	if err != nil {
		t.Fatalf("GET caps: %v", err)
	}
	defer resp.Body.Close()
	var caps struct {
		LocalShell bool `json:"local_shell"`
		Profiles   bool `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatalf("decode caps: %v", err)
	}
	if caps.LocalShell != console.LocalShellSupported() {
		t.Errorf("caps.local_shell = %v, want %v", caps.LocalShell, console.LocalShellSupported())
	}
	if !caps.Profiles {
		t.Error("caps.profiles should be true (store wired)")
	}
}

func TestConsoleProfiles_CRUD(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	// Create.
	body := `{"name":"VPS","host":"vps.example","port":2222,"username":"root","auth_kind":"password","secret":"hunter2"}`
	resp := env.postJSON(t, "/a/designer/console/profiles", body)
	var created struct {
		OK      bool                 `json:"ok"`
		Profile consolestore.Profile `json:"profile"`
	}
	decodeJSON(t, resp, &created)
	if !created.OK || created.Profile.ID == 0 {
		t.Fatalf("create failed: %+v", created)
	}
	id := created.Profile.ID

	// List — the secret must never come back, only the has_secret flag.
	listResp, err := env.client.Get(env.ts.URL + "/a/designer/console/profiles")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	raw, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("plaintext secret leaked into profile list")
	}
	if !strings.Contains(string(raw), `"has_secret":true`) {
		t.Fatalf("list missing has_secret flag: %s", raw)
	}

	// Update (rename, keep the stored secret).
	upd := `{"name":"VPS-Frankfurt","host":"vps.example","port":2222,"username":"root","auth_kind":"password"}`
	env.postJSON(t, "/a/designer/console/profiles/"+strconv.FormatInt(id, 10), upd)
	got, err := env.consoleStore.GetProfile(context.Background(), id)
	if err != nil || got.Name != "VPS-Frankfurt" {
		t.Fatalf("after update: %+v err=%v", got, err)
	}
	cred, err := env.consoleStore.ProfileCredential(context.Background(), id)
	if err != nil || cred.Password != "hunter2" {
		t.Fatalf("secret should survive rename: %q err=%v", cred.Password, err)
	}

	// Delete.
	env.postJSON(t, "/a/designer/console/profiles/"+strconv.FormatInt(id, 10)+"/delete", "")
	if _, err := env.consoleStore.GetProfile(context.Background(), id); err != consolestore.ErrNotFound {
		t.Fatalf("after delete err = %v, want ErrNotFound", err)
	}
}

// A failed SSH connect must never write the credential into any log line.
// The console manager is the component that logs session lifecycle; we
// capture its logger (race-free, since nothing else touches this fresh
// manager) and assert the secret never appears.
func TestConsoleWS_CredentialNeverLogged(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	buf := &syncBuffer{}
	env.srv.console = console.NewManager(
		slog.New(slog.NewTextHandler(buf, nil)), console.WithIdleTimeout(0))

	wsURL := strings.Replace(env.ts.URL, "http://", "ws://", 1) + "/a/designer/console/ws"
	conn, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPClient: &http.Client{Jar: env.client.Jar},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.CloseNow()

	const secret = "leaky-password-DEADBEEF"
	hello := `{"kind":"ssh","host":"127.0.0.1","port":1,"user":"u","auth":"password","password":"` + secret + `"}`
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(hello)); err != nil {
		t.Fatalf("ws write hello: %v", err)
	}
	// Expect a status:error control frame (dial to port 1 fails fast).
	_, data, err := conn.Read(context.Background())
	if err == nil && !bytes.Contains(data, []byte("error")) {
		_, _, _ = conn.Read(context.Background())
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("credential leaked into logs:\n%s", buf.String())
	}
}

// ---- helpers ----
// (syncBuffer, a goroutine-safe log sink, is defined in
// handler_esp_stream_test.go and reused here.)

func (e *testEnv) postJSON(t *testing.T, path, body string) *http.Response {
	t.Helper()
	resp, err := e.client.Post(e.ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST %s = %d: %s", path, resp.StatusCode, raw)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
