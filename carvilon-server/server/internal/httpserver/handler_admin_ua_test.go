package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/protectapi"
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
		env := func(data any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "SUCCESS", "msg": "ok", "data": data})
		}
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

	body := getBody(t, env, "/a/devices")
	if !strings.Contains(body, "UniFi Access is disabled") {
		t.Errorf("disabled hint missing:\n%s", firstLines(body))
	}
	if h := atomic.LoadInt32(&stub.hits); h != 0 {
		t.Errorf("UDM was called %d times while UA disabled, want 0", h)
	}
	if !strings.Contains(body, `href="/a/devices"`) {
		t.Errorf("UA nav link missing from topbar")
	}
}

// The pre-rename /a/ua* family 301s to /a/devices* - the page, the
// sub-paths a cached page still fetches, and the query string all
// survive the move. The redirect is method-scoped to GET.
func TestAdminUA_LegacyUARedirect(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	for _, tc := range []struct{ path, want string }{
		{"/a/ua", "/a/devices"},
		{"/a/ua/", "/a/devices"},
		{"/a/ua?flash=renamed", "/a/devices?flash=renamed"},
		{"/a/ua/status", "/a/devices/status"},
		{"/a/ua/doors/0cea-door-1", "/a/devices/doors/0cea-door-1"},
	} {
		resp, err := env.client.Get(env.ts.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusMovedPermanently || loc != tc.want {
			t.Errorf("GET %s = %d %q, want 301 %q", tc.path, resp.StatusCode, loc, tc.want)
		}
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

	body := getBody(t, env, "/a/devices")
	for _, want := range []string{
		"Haupt-Hub", "Leser Eingang", "Mock Cam", "UA Cam", // device names (data)
		"Hauseingang", "Kellertür", // door names (data)
		">Hubs<", ">Readers<", ">Viewers<", ">Doors<", // group headings (English)
		"CARVILON mock viewer",  // viewer-origin detail (mock)
		"Read only",             // read-only indicator
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

	body := getBody(t, env, "/a/devices")
	if !strings.Contains(body, "Access denied") {
		t.Errorf("401 card missing")
	}
	if strings.Contains(body, ts.URL) {
		t.Errorf("UDM host leaked into the page")
	}
}

// The live-status poll returns the counters plus one status item per
// device and door, keyed the way the client matches rows (kind+id).
func TestAdminUA_StatusSnapshot(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newUAStub(t)
	wireUA(t, env, stub)

	resp, err := env.client.Get(env.ts.URL + "/a/devices/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool `json:"ok"`
		Counts struct {
			Online  int `json:"online"`
			Offline int `json:"offline"`
			Total   int `json:"total"`
		} `json:"counts"`
		Items []struct {
			Kind   string `json:"kind"`
			ID     string `json:"id"`
			Status string `json:"status"`
			Text   string `json:"text"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK {
		t.Fatalf("ok=false: %+v", out)
	}
	// Stub: 3 devices online, 1 without is_online -> offline; 2 doors.
	if out.Counts.Online != 3 || out.Counts.Offline != 1 || out.Counts.Total != 6 {
		t.Errorf("counts = %+v, want online=3 offline=1 total=6", out.Counts)
	}
	byKey := map[string]string{}
	for _, it := range out.Items {
		byKey[it.Kind+"/"+it.ID] = it.Status
	}
	for key, want := range map[string]string{
		"device/0cea14000001": "online",
		"device/aabbccddee02": "offline",
		"door/0cea-door-1":    "locked",
		"door/0cea-door-2":    "unknown",
	} {
		if got := byKey[key]; got != want {
			t.Errorf("status[%s] = %q, want %q", key, got, want)
		}
	}
}

// UA off: the status poll answers ok:false and makes ZERO UDM calls.
func TestAdminUA_StatusDisabledMakesNoCalls(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newUAStub(t)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: stub.ts.URL, Token: "t"}))
	// KeyUAEnabled left unset + no token stored -> uaEnabled=false.

	resp, err := env.client.Get(env.ts.URL + "/a/devices/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.OK {
		t.Errorf("ok=true while UA disabled")
	}
	if h := atomic.LoadInt32(&stub.hits); h != 0 {
		t.Errorf("UDM was called %d times while UA disabled, want 0", h)
	}
}

// The lazy device-settings endpoint returns the flattened detail as JSON.
func TestAdminUA_DeviceSettingsLazy(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newUAStub(t)
	wireUA(t, env, stub)

	resp, err := env.client.Get(env.ts.URL + "/a/devices/devices/aabbccddee01/settings")
	if err != nil {
		t.Fatalf("GET settings: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK       bool `json:"ok"`
		Sections []struct {
			Title string                        `json:"title"`
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

	resp, err := env.client.Get(env.ts.URL + "/a/devices/doors/0cea-door-1")
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
		"/a/devices/devices/bad%20id!/settings",
		"/a/devices/devices/%2e%2e/settings", // %2e%2e -> ".." bypasses ServeMux cleaning
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
		{"0cea14000001", true},                         // bare MAC
		{"00000000-0000-0000-0000-000000000000", true}, // UUID
		{"door-1", true},
		{"", false},
		{"..", false},
		{"a..b", false},
		{"../../etc", false},
		{"a/b", false},                    // slash
		{"a b", false},                    // space
		{"a!b", false},                    // bang
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

// --- Protect Etappe 1 (cameras + sensors, read-only) ---

// protectStub is a fake Protect Integration API serving the read-only
// endpoints the Device Center uses. hits counts every request; fail
// flips the stub into a 500-everything mode.
type protectStub struct {
	ts   *httptest.Server
	hits int32
	fail bool
}

func newProtectStub(t *testing.T) *protectStub {
	t.Helper()
	s := &protectStub{}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.hits, 1)
		w.Header().Set("Content-Type", "application/json")
		if s.fail {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		if r.Header.Get("X-API-KEY") == "" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/proxy/protect/integration/v1/meta/info":
			_, _ = w.Write([]byte(`{"applicationVersion":"5.0.34"}`))
		case "/proxy/protect/integration/v1/cameras":
			// cam-2 carries only the identity trio - every optional
			// field must degrade to "-" without breaking anything.
			_, _ = w.Write([]byte(`[
				{"id":"cam-1","name":"Einfahrt","state":"CONNECTED","mac":"AABBCC001122","videoMode":"default","hdrType":"auto","hasPackageCamera":true,"featureFlags":{"hasHdr":true}},
				{"id":"cam-2","name":"Garten","state":"DISCONNECTED"}
			]`))
		case "/proxy/protect/integration/v1/sensors":
			_, _ = w.Write([]byte(`[
				{"id":"sen-1","name":"Kellersensor","state":"CONNECTED","mac":"AABBCC003344",
				 "mountType":"leak","isOpened":false,"isMotionDetected":false,
				 "stats":{"temperature":{"value":21.5},"humidity":{"value":48},"light":{"value":120}},
				 "batteryStatus":{"percentage":87,"isLow":false},
				 "wirelessConnectionState":{"signalState":"good","signalStrength":-62,"bridge":"bridge-1"}}
			]`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// wireProtect turns the "Protect aktiv" toggle on and points the
// client at the stub.
func wireProtect(t *testing.T, env *testEnv, stub *protectStub) {
	t.Helper()
	if err := env.platformCfg.Set(context.Background(), platformconfig.KeyProtectEnabled, "1"); err != nil {
		t.Fatalf("enable protect: %v", err)
	}
	env.srv.SetProtectClient(protectapi.New(protectapi.Options{BaseURL: stub.ts.URL, APIKey: "k"}))
}

// Protect on: cameras + sensors appear as real rows in their own
// groups, their facets are enabled, sensor readings land in the
// pre-rendered detail and missing camera fields degrade to "-".
func TestAdminUA_ProtectRows(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	wireUA(t, env, newUAStub(t))
	wireProtect(t, env, newProtectStub(t))

	body := getBody(t, env, "/a/devices")
	for _, want := range []string{
		"Einfahrt", "Garten", "Kellersensor", // protect device names
		">Cameras<", ">Sensors<", // group headings
		"Video mode", "HDR type", "Package camera", // camera panel fields
		"21.5 °C", "48 %", "120 lx", // sensor readings
		"Water leak", "Connected to", "bridge-1", // sensor panel fields
	} {
		if !strings.Contains(body, want) {
			t.Errorf("protect overview missing %q", want)
		}
	}
	// The facets are real now - not the disabled shells.
	for _, facet := range []string{`data-dc-value="camera"`, `data-dc-value="sensor"`} {
		if !strings.Contains(body, facet) {
			t.Errorf("facet %s missing", facet)
		}
		if strings.Contains(body, facet+" disabled") {
			t.Errorf("facet %s still disabled", facet)
		}
	}
}

// UA off but Protect on: the table renders (no gate card), a notice
// banner explains, and the UA developer API is never called.
func TestAdminUA_ProtectOnlyMakesNoUACalls(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	uaStub := newUAStub(t)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.ts.URL, Token: "t"}))
	// KeyUAEnabled left unset + no token stored -> uaEnabled=false.
	wireProtect(t, env, newProtectStub(t))

	body := getBody(t, env, "/a/devices")
	// `class="dc-gate` is the rendered card; the bare ".dc-gate" in
	// the stylesheet is always present.
	if strings.Contains(body, `class="dc-gate`) {
		t.Errorf("gate card shown although Protect fills the page")
	}
	for _, want := range []string{"Einfahrt", "Kellersensor", "only devices from other sources are shown"} {
		if !strings.Contains(body, want) {
			t.Errorf("protect-only overview missing %q", want)
		}
	}
	if h := atomic.LoadInt32(&uaStub.hits); h != 0 {
		t.Errorf("UDM developer API was called %d times while UA disabled, want 0", h)
	}
}

// A Protect failure keeps the UA rows and degrades to a banner - one
// broken source never blanks the page.
func TestAdminUA_ProtectErrorKeepsPage(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	wireUA(t, env, newUAStub(t))
	stub := newProtectStub(t)
	stub.fail = true
	wireProtect(t, env, stub)

	body := getBody(t, env, "/a/devices")
	for _, want := range []string{"Haupt-Hub", "Cameras and sensors could not be loaded"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	if strings.Contains(body, stub.ts.URL) {
		t.Errorf("protect host leaked into the page")
	}
}

// The lazy camera endpoint returns the flattened full record so the
// panel shows everything the NVR sent (featureFlags included).
func TestAdminUA_ProtectCameraDetailLazy(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	wireUA(t, env, newUAStub(t))
	wireProtect(t, env, newProtectStub(t))

	resp, err := env.client.Get(env.ts.URL + "/a/devices/protect/cameras/cam-1")
	if err != nil {
		t.Fatalf("GET camera detail: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK       bool `json:"ok"`
		Sections []struct {
			Title string                        `json:"title"`
			Rows  []struct{ Key, Value string } `json:"rows"`
		} `json:"sections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || len(out.Sections) != 1 || out.Sections[0].Title != "Camera details" {
		t.Fatalf("bad response: %+v", out)
	}
	var sawFlag bool
	for _, kv := range out.Sections[0].Rows {
		if kv.Key == "featureFlags.hasHdr" && kv.Value == "Yes" {
			sawFlag = true
		}
	}
	if !sawFlag {
		t.Errorf("featureFlags.hasHdr=Yes row missing: %+v", out.Sections[0].Rows)
	}
}

// The status poll covers the Protect rows too and counts them into
// the fleet counters.
func TestAdminUA_StatusIncludesProtect(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	wireUA(t, env, newUAStub(t))
	wireProtect(t, env, newProtectStub(t))

	resp, err := env.client.Get(env.ts.URL + "/a/devices/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool `json:"ok"`
		Counts struct {
			Online  int `json:"online"`
			Offline int `json:"offline"`
			Total   int `json:"total"`
		} `json:"counts"`
		Items []struct {
			Kind   string `json:"kind"`
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK {
		t.Fatalf("ok=false")
	}
	// UA: 3 online + 1 offline devices + 2 doors; Protect: 1 online +
	// 1 offline camera + 1 online sensor.
	if out.Counts.Online != 5 || out.Counts.Offline != 2 || out.Counts.Total != 9 {
		t.Errorf("counts = %+v, want online=5 offline=2 total=9", out.Counts)
	}
	byKey := map[string]string{}
	for _, it := range out.Items {
		byKey[it.Kind+"/"+it.ID] = it.Status
	}
	if byKey["camera/cam-2"] != "offline" || byKey["sensor/sen-1"] != "online" {
		t.Errorf("protect statuses wrong: %v", byKey)
	}
}

// POST /a/settings/protect stores the key encrypted; the settings page
// only ever shows "gesetzt", never the key itself.
func TestAdminSettings_ProtectStoresKeyEncrypted(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	resp, err := env.client.PostForm(env.ts.URL+"/a/settings/protect", url.Values{
		"protect_controller_url": {"https://protect.local"},
		"protect_api_key":        {"super-secret-test-key"},
		"protect_enabled":        {"1"},
	})
	if err != nil {
		t.Fatalf("POST protect settings: %v", err)
	}
	body := followSettings(t, env, resp)
	resp.Body.Close()
	if strings.Contains(body, "super-secret-test-key") {
		t.Errorf("API key echoed back into the fragment")
	}

	stored, err := env.platformCfg.GetSecret(context.Background(), platformconfig.KeyProtectAPIKey)
	if err != nil || stored != "super-secret-test-key" {
		t.Errorf("stored key = %q, err=%v", stored, err)
	}
	if v, _ := env.platformCfg.Get(context.Background(), platformconfig.KeyProtectEnabled); v != "1" {
		t.Errorf("protect_enabled = %q, want 1", v)
	}
	// The plaintext column must NOT hold the key (it lives encrypted).
	if v, _ := env.platformCfg.Get(context.Background(), platformconfig.KeyProtectAPIKey); v == "super-secret-test-key" {
		t.Errorf("API key stored in plaintext")
	}

	page := getBody(t, env, "/a/settings/panel/protect")
	if !strings.Contains(page, "API key") || !strings.Contains(page, "* * * (set)") {
		t.Errorf("protect settings fragment incomplete")
	}
	if strings.Contains(page, "super-secret-test-key") {
		t.Errorf("API key leaked into the settings fragment")
	}
}
