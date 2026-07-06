package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/shellyapi"
	"carvilon.local/server/internal/uaapi"
)

// shellyStub is a fake Gen2 device serving POST /rpc for the three
// read-only methods. hits counts every request so tests can assert
// the handler never dials what it must not.
type shellyStub struct {
	ts   *httptest.Server
	hits int32
}

func (s *shellyStub) addr() string { return strings.TrimPrefix(s.ts.URL, "http://") }

func newShellyStub(t *testing.T, name string) *shellyStub {
	t.Helper()
	s := &shellyStub{}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.hits, 1)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		result := ""
		switch req.Method {
		case "Shelly.GetDeviceInfo":
			result = `{"id":"shellypro4pm-08f9e0e5c790","name":"` + name + `","model":"SPSW-104PE16EU","mac":"08F9E0E5C790","app":"Pro4PM","ver":"1.4.4","gen":2,"auth_en":false}`
		case "Shelly.GetStatus":
			result = `{
				"switch:0":{"id":0,"output":true,"apower":52.3,"voltage":230.1,"current":0.229,"freq":50.0,"aenergy":{"total":32406.879}},
				"switch:1":{"id":1,"output":false,"apower":0.0,"voltage":231.0,"current":0.0,"freq":50.0,"aenergy":{"total":812.4}},
				"input:0":{"id":0,"state":false},
				"sys":{"uptime":4711}
			}`
		case "Shelly.GetConfig":
			result = `{"switch:0":{"id":0,"name":"SANlight One"},"switch:1":{"id":1,"name":null},"input:0":{"id":0,"name":null}}`
		default:
			_, _ = w.Write([]byte(`{"id":1,"error":{"code":404,"message":"no handler"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":1,"src":"shellypro4pm-08f9e0e5c790","result":` + result + `}`))
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// deadAddr returns a loopback address nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// wireShelly turns the Shelly toggle on and points one client per
// address at the given targets (mirror of wireProtect).
func wireShelly(t *testing.T, env *testEnv, addrs ...string) {
	t.Helper()
	if err := env.platformCfg.Set(context.Background(), platformconfig.KeyShellyEnabled, "1"); err != nil {
		t.Fatalf("enable shelly: %v", err)
	}
	clients := make([]*shellyapi.Client, 0, len(addrs))
	for _, a := range addrs {
		clients = append(clients, shellyapi.New(shellyapi.Options{Address: a}))
	}
	env.srv.SetShellyClients(clients)
}

// Shelly on: devices appear as real rows in the Switches group with
// source "Shelly"; an unreachable device renders as an offline row
// under its configured address and the page holds.
func TestAdminUA_ShellyRows(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	wireUA(t, env, newUAStub(t))
	stub := newShellyStub(t, "Growbox")
	dead := deadAddr(t)
	wireShelly(t, env, stub.addr(), dead)

	body := getBody(t, env, "/a/ua")
	for _, want := range []string{
		">Switches<",              // group heading
		"Growbox",                 // device name from GetDeviceInfo
		"Shelly Pro4PM",           // model label
		"08F9E0E5C790",            // MAC in the row
		`data-dc-value="shelly"`,  // source facet
		`data-source="shelly"`,    // row source key
		`data-kind="shelly"`,      // row kind for the lazy panel
		dead,                      // the dead device keeps its row (address as name)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shelly overview missing %q", want)
		}
	}
	// The dead device's row is offline; the live one online.
	if !strings.Contains(body, `data-id="`+dead+`" data-kind`) && !strings.Contains(body, `data-status="offline"`) {
		t.Errorf("offline shelly row missing")
	}
}

// UA and Protect off, Shelly on: the table renders (no gate card),
// the notice banner explains, and the UA developer API is never
// called - the page must not be coupled to any UniFi integration.
func TestAdminUA_ShellyOnlyRendersTable(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	uaStub := newUAStub(t)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.ts.URL, Token: "t"}))
	// KeyUAEnabled left unset + no token stored -> uaEnabled=false.
	stub := newShellyStub(t, "Growbox")
	wireShelly(t, env, stub.addr())

	body := getBody(t, env, "/a/ua")
	if strings.Contains(body, `class="dc-gate`) {
		t.Errorf("gate card shown although Shelly fills the page")
	}
	for _, want := range []string{"Growbox", ">Switches<", "only devices from other sources are shown"} {
		if !strings.Contains(body, want) {
			t.Errorf("shelly-only overview missing %q", want)
		}
	}
	if h := atomic.LoadInt32(&uaStub.hits); h != 0 {
		t.Errorf("UDM developer API was called %d times while UA disabled, want 0", h)
	}
}

// The lazy detail endpoint returns one section per switch channel
// (named from the config) with the briefed measurements, plus the
// inputs - fetched live from the device.
func TestAdminUA_ShellyDetailLazy(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newShellyStub(t, "Growbox")
	wireShelly(t, env, stub.addr())

	resp, err := env.client.Get(env.ts.URL + "/a/ua/shelly/" + url.PathEscape(stub.addr()))
	if err != nil {
		t.Fatalf("GET shelly detail: %v", err)
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
	if !out.OK || len(out.Sections) != 3 {
		t.Fatalf("bad response: %+v", out)
	}
	if out.Sections[0].Title != "Switch 1 · SANlight One" || out.Sections[1].Title != "Switch 2" || out.Sections[2].Title != "Inputs" {
		t.Errorf("section titles: %q / %q / %q", out.Sections[0].Title, out.Sections[1].Title, out.Sections[2].Title)
	}
	want := map[string]string{
		"State": "On", "Power": "52.3 W", "Voltage": "230.1 V",
		"Current": "0.229 A", "Frequency": "50.0 Hz", "Energy": "32406.879 Wh",
	}
	got := map[string]string{}
	for _, kv := range out.Sections[0].Rows {
		got[kv.Key] = kv.Value
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("channel 1 %s = %q, want %q", k, got[k], v)
		}
	}
	if len(out.Sections[2].Rows) != 1 || out.Sections[2].Rows[0].Key != "Input 1" || out.Sections[2].Rows[0].Value != "Off" {
		t.Errorf("inputs section wrong: %+v", out.Sections[2].Rows)
	}
}

// The lazy endpoint only ever dials CONFIGURED addresses: a valid but
// unconfigured address answers "Not found." and the foreign target is
// never contacted (SSRF guard).
func TestAdminUA_ShellyDetailOnlyDialsConfigured(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	configured := newShellyStub(t, "Growbox")
	foreign := newShellyStub(t, "Foreign")
	wireShelly(t, env, configured.addr())

	resp, err := env.client.Get(env.ts.URL + "/a/ua/shelly/" + url.PathEscape(foreign.addr()))
	if err != nil {
		t.Fatalf("GET foreign detail: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":false`) || !strings.Contains(string(body), "Not found.") {
		t.Errorf("foreign address not refused: %s", body)
	}
	if h := atomic.LoadInt32(&foreign.hits); h != 0 {
		t.Errorf("foreign target was dialed %d times, want 0", h)
	}
	// Bad id shapes are rejected before any lookup.
	resp2, err := env.client.Get(env.ts.URL + "/a/ua/shelly/" + url.PathEscape("../evil"))
	if err != nil {
		t.Fatalf("GET bad id: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id status = %d, want 400", resp2.StatusCode)
	}
}

// The status poll covers the Shelly rows (kind+address) and counts
// them into the fleet counters; a dead device reads offline without
// suppressing the counters.
func TestAdminUA_StatusIncludesShelly(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newShellyStub(t, "Growbox")
	dead := deadAddr(t)
	wireShelly(t, env, stub.addr(), dead)

	resp, err := env.client.Get(env.ts.URL + "/a/ua/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK      bool            `json:"ok"`
		Sources map[string]bool `json:"sources"`
		Counts  struct {
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
	if !out.OK || !out.Sources["shelly"] {
		t.Fatalf("ok/sources wrong: %+v", out)
	}
	if out.Counts.Online != 1 || out.Counts.Offline != 1 || out.Counts.Total != 2 {
		t.Errorf("counts = %+v, want online=1 offline=1 total=2", out.Counts)
	}
	byKey := map[string]string{}
	for _, it := range out.Items {
		byKey[it.Kind+"/"+it.ID] = it.Status
	}
	if byKey["shelly/"+stub.addr()] != "online" || byKey["shelly/"+dead] != "offline" {
		t.Errorf("shelly statuses wrong: %v", byKey)
	}
}

// POST /a/settings/shelly stores the password encrypted and the
// validated address list; the settings page shows the block, renders
// the addresses back and never echoes the password.
func TestAdminSettings_ShellyStoresPasswordEncrypted(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	resp, err := env.client.PostForm(env.ts.URL+"/a/settings/shelly", url.Values{
		"shelly_addresses": {"http://192.168.33.51/, 192.168.33.52:8080, 192.168.33.51"},
		"shelly_password":  {"super-secret-shelly-pw"},
		"shelly_enabled":   {"1"},
	})
	if err != nil {
		t.Fatalf("POST shelly settings: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Contains(string(body), "super-secret-shelly-pw") {
		t.Errorf("password echoed back into the page")
	}

	// Normalised, deduped list (URL form stripped, port kept).
	if v, _ := env.platformCfg.Get(context.Background(), platformconfig.KeyShellyAddresses); v != "192.168.33.51, 192.168.33.52:8080" {
		t.Errorf("stored addresses = %q", v)
	}
	stored, err := env.platformCfg.GetSecret(context.Background(), platformconfig.KeyShellyPassword)
	if err != nil || stored != "super-secret-shelly-pw" {
		t.Errorf("stored password = %q, err=%v", stored, err)
	}
	if v, _ := env.platformCfg.Get(context.Background(), platformconfig.KeyShellyPassword); v == "super-secret-shelly-pw" {
		t.Errorf("password stored in plaintext")
	}
	if v, _ := env.platformCfg.Get(context.Background(), platformconfig.KeyShellyEnabled); v != "1" {
		t.Errorf("shelly_enabled = %q, want 1", v)
	}
	// The POST rebuilt the fleet from the stored config.
	if n := len(env.srv.shellyClientList()); n != 2 {
		t.Errorf("fleet size after save = %d, want 2", n)
	}

	page := getBody(t, env, "/a/settings")
	if !strings.Contains(page, "Shelly Integration") || !strings.Contains(page, "* * * (set)") {
		t.Errorf("shelly settings section incomplete")
	}
	if !strings.Contains(page, "192.168.33.51, 192.168.33.52:8080") {
		t.Errorf("addresses not rendered back into the form")
	}
	if strings.Contains(page, "super-secret-shelly-pw") {
		t.Errorf("password leaked into the settings page")
	}
}

// Invalid or non-LAN addresses are refused with a red flash and the
// stored configuration stays untouched.
func TestAdminSettings_ShellyRejectsBadAddresses(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	if err := env.platformCfg.Set(context.Background(), platformconfig.KeyShellyAddresses, "192.168.33.51"); err != nil {
		t.Fatalf("seed addresses: %v", err)
	}

	for _, bad := range []string{
		"8.8.8.8",             // public
		"169.254.169.254",     // cloud metadata
		"192.168.33.51:99999", // port out of range
		"shelly.local",        // hostname (IPs only in Etappe 1)
		"2001:db8::1",         // not IPv4
	} {
		resp, err := env.client.PostForm(env.ts.URL+"/a/settings/shelly", url.Values{
			"shelly_addresses": {bad},
			"shelly_enabled":   {"1"},
		})
		if err != nil {
			t.Fatalf("POST %q: %v", bad, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "Device addresses:") {
			t.Errorf("%q: validation flash missing", bad)
		}
		if v, _ := env.platformCfg.Get(context.Background(), platformconfig.KeyShellyAddresses); v != "192.168.33.51" {
			t.Errorf("%q: stored addresses changed to %q", bad, v)
		}
	}
}

// The settings "Connection" probe reports counts only - never an
// address - and probes only when enabled.
func TestAdminSettings_ShellyConnectionStatus(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newShellyStub(t, "Growbox")
	dead := deadAddr(t)
	wireShelly(t, env, stub.addr(), dead)

	body := getBody(t, env, "/a/settings/shelly/status")
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["ok"] != true || out["enabled"] != true || out["total"] != float64(2) || out["reachable"] != float64(1) {
		t.Errorf("status = %v", out)
	}
	if strings.Contains(body, stub.addr()) || strings.Contains(body, dead) {
		t.Errorf("status JSON leaked an address: %s", body)
	}

	// Disabled: no probe, no reachable field.
	if err := env.platformCfg.Set(context.Background(), platformconfig.KeyShellyEnabled, "0"); err != nil {
		t.Fatalf("disable shelly: %v", err)
	}
	before := atomic.LoadInt32(&stub.hits)
	body = getBody(t, env, "/a/settings/shelly/status")
	if !strings.Contains(body, `"enabled":false`) || strings.Contains(body, "reachable") {
		t.Errorf("disabled status = %s", body)
	}
	if atomic.LoadInt32(&stub.hits) != before {
		t.Errorf("disabled status still probed the device")
	}
}

func TestParseShellyAddresses(t *testing.T) {
	// Canonicalisation: URL forms strip to host, ":80" and a trailing
	// ":" fold into the bare form, and equivalent spellings dedupe.
	got, err := parseShellyAddresses(" http://10.1.2.3/ ,10.1.2.4:8080;10.1.2.3:80\n127.0.0.1:18101 10.1.2.3:")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"10.1.2.3", "10.1.2.4:8080", "127.0.0.1:18101"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
	if list, err := parseShellyAddresses(""); err != nil || len(list) != 0 {
		t.Errorf("empty input: %v / %v", list, err)
	}
	for _, bad := range []string{
		"10.1.2.3:0",       // port out of range
		"10.1.2.3:080",     // non-canonical port
		"10.1.2.3:+80",     // signed port
		"::ffff:10.1.2.3",  // IPv4-mapped IPv6 text (dial path can't use it)
		"010.1.2.3",        // non-canonical host spelling
	} {
		if _, err := parseShellyAddresses(bad); err == nil {
			t.Errorf("%q accepted, want error", bad)
		}
	}
}
