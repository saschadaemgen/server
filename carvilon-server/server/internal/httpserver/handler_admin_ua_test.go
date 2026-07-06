package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/viewerstore"
)

// uaStub is a fake UDM developer API serving the read-only endpoints
// the overview uses. hits counts every request so tests can assert the
// page makes no calls when UA is disabled.
type uaStub struct {
	ts   *httptest.Server
	hits int32
}

func newUAStub(t *testing.T) *uaStub {
	t.Helper()
	s := &uaStub{}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.hits, 1)
		w.Header().Set("Content-Type", "application/json")
		env := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"code": "SUCCESS", "msg": "ok", "data": data}) }
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/developer/devices"):
			env([]map[string]any{
				{"id": "0cea14000001", "alias": "Haupt-Hub", "type": "UAH-DOOR", "is_online": true, "is_adopted": true, "capabilities": []string{"is_hub"}},
				{"id": "0cea14000002", "alias": "Leser Eingang", "type": "UA-G2-Reader", "is_online": true, "connected_uah_id": "0cea14000001", "capabilities": []string{"is_reader"}},
				{"id": "aabbccddee01", "alias": "Mock Cam", "type": "UVC", "is_online": true, "connected_uah_id": "0cea14000001", "capabilities": []string{"support_continuous_monitoring"}},
				{"id": "aabbccddee02", "alias": "UA Cam", "type": "UVC", "connected_uah_id": "0cea14000001", "capabilities": []string{"support_continuous_monitoring"}},
			})
		case strings.HasSuffix(p, "/developer/doors/settings/emergency"):
			env(map[string]any{"lockdown": false, "evacuation": false})
		case strings.HasSuffix(p, "/developer/doors/0cea-door-1/lock_rule"):
			env(map[string]any{"type": "schedule", "name": "Bürozeiten"})
		case strings.HasSuffix(p, "/developer/doors/0cea-door-1"):
			env(map[string]any{"id": "0cea-door-1", "name": "Hauseingang", "hub_id": "0cea14000001", "door_position_status": "open"})
		case strings.HasSuffix(p, "/developer/doors"):
			env([]map[string]any{
				{"id": "0cea-door-1", "name": "Hauseingang", "hub_id": "0cea14000001", "door_position_status": "open", "door_lock_relay_status": "lock", "is_bind_hub": true},
				{"id": "0cea-door-2", "name": "Kellertür", "door_position_status": "close"},
			})
		case strings.HasSuffix(p, "/settings"): // /devices/{id}/settings
			env(map[string]any{"nfc": true, "bt_tap": false})
		default:
			env(nil)
		}
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// wireUA turns the "UA aktiv" toggle on and points the client at the stub.
func wireUA(t *testing.T, env *testEnv, stub *uaStub) {
	t.Helper()
	enableUA(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: stub.ts.URL, Token: "t"}))
}

// UA off: the page shows the disabled hint and makes ZERO calls to the UDM.
func TestAdminUA_DisabledMakesNoCalls(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newUAStub(t)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: stub.ts.URL, Token: "t"}))
	// KeyUAEnabled left unset + no token stored -> uaEnabled=false.

	body := getBody(t, env, "/a/ua")
	if !strings.Contains(body, "UniFi Access is disabled") {
		t.Errorf("disabled hint missing:\n%s", firstLines(body))
	}
	if h := atomic.LoadInt32(&stub.hits); h != 0 {
		t.Errorf("UDM was called %d times while UA disabled, want 0", h)
	}
	if !strings.Contains(body, `href="/a/ua"`) {
		t.Errorf("UA nav link missing from topbar")
	}
}

// UA on + reachable: the flat device table lists every device + door,
// grouped by category, mock vs UA viewers are distinguished, and doors
// carry the lock/position status.
func TestAdminUA_FlatOverview(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newUAStub(t)

	// Seed one mock viewer whose MAC matches a UA-reported viewer.
	if err := viewerstore.Insert(context.Background(), env.d.DB, viewerstore.InsertSpec{
		MAC: "aa:bb:cc:dd:ee:01", Name: "Wohnzimmer", ServicePort: 8100, Type: "web",
	}, 1); err != nil {
		t.Fatalf("seed viewer: %v", err)
	}
	wireUA(t, env, stub)

	body := getBody(t, env, "/a/ua")
	for _, want := range []string{
		"Haupt-Hub", "Leser Eingang", "Mock Cam", "UA Cam", // device names (data)
		"Hauseingang", "Kellertür", // door names (data)
		">Hubs<", ">Readers<", ">Viewers<", ">Doors<", // group headings (English)
		"CARVILON mock viewer", // viewer-origin detail (mock)
		"Read only",            // read-only indicator
		"Fleet status", "UniFi", // left column + source facet
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	// Mock detection must be exact: the UA-reported "UA Cam" (aabbccddee02)
	// is NOT in the viewers table, so exactly one viewer carries the mock
	// badge.
	if n := strings.Count(body, `class="dc-mock"`); n != 1 {
		t.Errorf("mock badge count = %d, want 1", n)
	}
	// Cameras/Sensors are shell-only until a Protect backend exists: their
	// facets render disabled and no invented rows appear.
	if !strings.Contains(body, `data-dc-value="camera"`) || !strings.Contains(body, `data-dc-value="sensor"`) {
		t.Errorf("camera/sensor shell facets missing")
	}
}

// A bad token surfaces a clean 401 card, never a raw error or the host.
func TestAdminUA_Unauthorized(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"code":"CODE_UNAUTHORIZED","msg":"no","data":null}`))
	}))
	defer ts.Close()
	if err := env.platformCfg.Set(context.Background(), platformconfig.KeyUAEnabled, "1"); err != nil {
		t.Fatalf("set ua_enabled: %v", err)
	}
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: ts.URL, Token: "bad"}))

	body := getBody(t, env, "/a/ua")
	if !strings.Contains(body, "Access denied") {
		t.Errorf("401 card missing")
	}
	if strings.Contains(body, ts.URL) {
		t.Errorf("UDM host leaked into the page")
	}
}

// The lazy device-settings endpoint returns the flattened detail as JSON.
func TestAdminUA_DeviceSettingsLazy(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newUAStub(t)
	wireUA(t, env, stub)

	resp, err := env.client.Get(env.ts.URL + "/a/ua/devices/aabbccddee01/settings")
	if err != nil {
		t.Fatalf("GET settings: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK       bool `json:"ok"`
		Sections []struct {
			Title string `json:"title"`
			Rows  []struct{ Key, Value string } `json:"rows"`
		} `json:"sections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || len(out.Sections) != 1 {
		t.Fatalf("bad response: %+v", out)
	}
	if out.Sections[0].Title != "Access methods" {
		t.Errorf("section title = %q, want %q", out.Sections[0].Title, "Access methods")
	}
	var sawNFC bool
	for _, kv := range out.Sections[0].Rows {
		if kv.Key == "nfc" && kv.Value == "Yes" {
			sawNFC = true
		}
	}
	if !sawNFC {
		t.Errorf("nfc=Yes row missing: %+v", out.Sections[0].Rows)
	}
}

// The lazy door endpoint returns both the door detail and its lock rule.
func TestAdminUA_DoorDetailLazy(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newUAStub(t)
	wireUA(t, env, stub)

	resp, err := env.client.Get(env.ts.URL + "/a/ua/doors/0cea-door-1")
	if err != nil {
		t.Fatalf("GET door: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK       bool `json:"ok"`
		Sections []struct {
			Title string `json:"title"`
		} `json:"sections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || len(out.Sections) != 2 {
		t.Fatalf("want detail+lock-rule sections, got %+v", out)
	}
	if out.Sections[0].Title != "Door details" || out.Sections[1].Title != "Lock rule" {
		t.Errorf("section titles = %q/%q", out.Sections[0].Title, out.Sections[1].Title)
	}
}

// The lazy endpoints reject a malformed id before any upstream call.
func TestAdminUA_DetailRejectsBadID(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newUAStub(t)
	wireUA(t, env, stub)

	// A malformed id (space + bang) and a percent-encoded traversal
	// attempt both reject with 400 before any upstream call. %2e%2e
	// decodes to ".." in PathValue and bypasses ServeMux path cleaning,
	// so it must be caught by uaValidID.
	for _, bad := range []string{
		"/a/ua/devices/bad%20id!/settings",
		"/a/ua/devices/%2e%2e/settings", // %2e%2e -> ".." bypasses ServeMux cleaning
	} {
		resp, err := env.client.Get(env.ts.URL + bad)
		if err != nil {
			t.Fatalf("GET %s: %v", bad, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, want 400", bad, resp.StatusCode)
		}
	}
	if h := atomic.LoadInt32(&stub.hits); h != 0 {
		t.Errorf("bad ids reached the UDM %d times, want 0", h)
	}
}

func TestUAValidID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"0cea14000001", true},        // bare MAC
		{"00000000-0000-0000-0000-000000000000", true}, // UUID
		{"door-1", true},
		{"", false},
		{"..", false},
		{"a..b", false},
		{"../../etc", false},
		{"a/b", false},   // slash
		{"a b", false},   // space
		{"a!b", false},   // bang
		{strings.Repeat("a", 129), false}, // too long
	}
	for _, c := range cases {
		if got := uaValidID(c.id); got != c.want {
			t.Errorf("uaValidID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

func firstLines(s string) string {
	if len(s) > 600 {
		return s[:600]
	}
	return s
}
