package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/shelly1api"
	"carvilon.local/server/internal/shellystore"
)

// shelly1Stub is a fake Gen1 device serving the frozen REST endpoints
// the Device Center reads (GET /shelly, /status, /settings) - the Gen1
// sibling of shellyStub. It models a Shelly 2.5 (SHSW-25) in relay
// mode: two metered relays, two inputs, a temperature block. hits
// counts every request so tests can assert the handler never dials
// what it must not; protected simulates device-side HTTP auth, where
// everything EXCEPT the documented unauthenticated /shelly identify
// endpoint answers 401 (set at construction - the field is read from
// the server goroutine).
type shelly1Stub struct {
	ts        *httptest.Server
	hits      int32
	protected bool
}

func (s *shelly1Stub) addr() string { return strings.TrimPrefix(s.ts.URL, "http://") }

func newShelly1Stub(t *testing.T, protected bool) *shelly1Stub {
	t.Helper()
	s := &shelly1Stub{protected: protected}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.hits, 1)
		if s.protected && r.URL.Path != "/shelly" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/shelly":
			// The identify answer has NO "gen" field - that absence is the
			// documented Gen1 signature the classifier relies on.
			_, _ = w.Write([]byte(`{"type":"SHSW-25","mac":"A4CF12E4B7C1","auth":false,"fw":"20230913-114010/v1.14.0-gcb84623","longid":1}`))
		case "/status":
			// meters[].total is WATT-MINUTES (600 -> 10 Wh) - the unit
			// conversion is part of what the detail test pins down.
			_, _ = w.Write([]byte(`{
				"relays":[{"ison":true},{"ison":false}],
				"meters":[{"power":41.5,"total":600},{"power":0,"total":0}],
				"inputs":[{"input":1},{"input":0}],
				"tmp":{"tC":48.2},
				"mqtt":{"connected":true}
			}`))
		case "/settings":
			_, _ = w.Write([]byte(`{
				"device":{"type":"SHSW-25","mac":"A4CF12E4B7C1"},
				"mode":"relay",
				"relays":[{"name":"Left"},{"name":"Right"}],
				"mqtt":{"enable":true},
				"login":{"enabled":false}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// wireShelly1 mirrors wireShelly for Gen1 devices: toggle on, one REST
// client per address, generation pinned to Gen1 so the fleet dispatches
// onto the shelly1api transport (never /rpc).
func wireShelly1(t *testing.T, env *testEnv, addrs ...string) {
	t.Helper()
	if err := env.platformCfg.Set(context.Background(), platformconfig.KeyShellyEnabled, "1"); err != nil {
		t.Fatalf("enable shelly: %v", err)
	}
	clients := make([]ShellyDeviceClient, 0, len(addrs))
	for _, a := range addrs {
		clients = append(clients, ShellyDeviceClient{
			Gen:  shellystore.Gen1,
			Gen1: shelly1api.New(shelly1api.Options{Address: a}),
		})
	}
	env.srv.SetShellyClients(clients)
}

// A Gen1 device renders as a real Switches row: the identify probe (GET
// /shelly - the only endpoint the overview may touch) supplies the
// human model label, MAC and firmware, and the row carries data-gen="1"
// for the panel's generation dispatch. With no store row there is no
// broker account and no store-side MAC to derive a topic prefix from,
// so data-prefix stays honestly empty - makeShellyRow builds the
// shellies/... prefix only from STORE facts, never from the live answer.
func TestAdminUA_ShellyGen1Row(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	wireUA(t, env, newUAStub(t))
	stub := newShelly1Stub(t, false)
	wireShelly1(t, env, stub.addr())

	body := getBody(t, env, "/a/devices")
	for _, want := range []string{
		">Switches<",                       // group heading (Gen1 shares the switch category)
		"Shelly 2.5",                       // SHSW-25 mapped to its human label
		"A4CF12E4B7C1",                     // MAC from the identify answer
		"20230913-114010/v1.14.0-gcb84623", // firmware label
		`data-gen="1" data-prefix=""`,      // generation tag + honest empty prefix
		`data-source="shelly"`,             // row source key
		`data-kind="shelly"`,               // row kind for the lazy panel
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gen1 overview missing %q", want)
		}
	}
	// The raw type code must not leak into the page - the row shows the
	// human label ("Shelly 2.5"), the code stays a capability-table key.
	if strings.Contains(body, "SHSW-25") {
		t.Errorf("raw Gen1 type code leaked into the overview")
	}
}

// The lazy Gen1 detail is served from GET /status with the channel
// names and the capability shape from GET /settings: one section per
// relay (metered on a 2.5, watt-minutes converted to Wh), the inputs,
// and the device block (temperature + the device's own MQTT view).
func TestAdminUA_ShellyGen1Detail(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newShelly1Stub(t, false)
	wireShelly1(t, env, stub.addr())

	resp, err := env.client.Get(env.ts.URL + "/a/devices/shelly/" + url.PathEscape(stub.addr()))
	if err != nil {
		t.Fatalf("GET gen1 detail: %v", err)
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
	if !out.OK || len(out.Sections) != 4 {
		t.Fatalf("bad response: %+v", out)
	}
	for i, want := range []string{"Relay 1 · Left", "Relay 2 · Right", "Inputs", "Device"} {
		if out.Sections[i].Title != want {
			t.Errorf("section %d title = %q, want %q", i, out.Sections[i].Title, want)
		}
	}
	rowsOf := func(i int) map[string]string {
		m := map[string]string{}
		for _, kv := range out.Sections[i].Rows {
			m[kv.Key] = kv.Value
		}
		return m
	}
	// Relay 1: metered per the SHSW-25 capability row; the 600
	// watt-minute counter renders as 10 Wh (the honest UI unit).
	for k, v := range map[string]string{"State": "On", "Power": "41.5 W", "Energy": "10 Wh"} {
		if got := rowsOf(0)[k]; got != v {
			t.Errorf("relay 1 %s = %q, want %q", k, got, v)
		}
	}
	// Relay 2: off, and its zero readings are real measurements - they
	// must render as numbers, not degrade to "-".
	for k, v := range map[string]string{"State": "Off", "Power": "0 W", "Energy": "0 Wh"} {
		if got := rowsOf(1)[k]; got != v {
			t.Errorf("relay 2 %s = %q, want %q", k, got, v)
		}
	}
	// Inputs are the physical switch terminals, 1-based like the relays.
	for k, v := range map[string]string{"Input 1": "On", "Input 2": "Off"} {
		if got := rowsOf(2)[k]; got != v {
			t.Errorf("inputs %s = %q, want %q", k, got, v)
		}
	}
	for k, v := range map[string]string{"Temperature": "48.2 °C", "MQTT (device view)": "Connected"} {
		if got := rowsOf(3)[k]; got != v {
			t.Errorf("device %s = %q, want %q", k, got, v)
		}
	}
}

// Device-side HTTP auth on and our credential missing: the panel
// answers with the fixed friendly auth message, never the transport
// error (whose text could carry the URL/address), and the handler
// stops after the failed /status - no pointless second authenticated
// call to /settings.
func TestAdminUA_ShellyGen1DetailUnauthorized(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newShelly1Stub(t, true)
	wireShelly1(t, env, stub.addr())

	resp, err := env.client.Get(env.ts.URL + "/a/devices/shelly/" + url.PathEscape(stub.addr()))
	if err != nil {
		t.Fatalf("GET gen1 detail: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), stub.addr()) {
		t.Errorf("device address leaked into the detail JSON")
	}
	var out struct {
		OK       bool `json:"ok"`
		Sections []struct {
			Title string `json:"title"`
			Error string `json:"error"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || len(out.Sections) != 1 {
		t.Fatalf("bad response: %s", body)
	}
	if out.Sections[0].Title != "Relays" ||
		out.Sections[0].Error != "Access denied - please check the Shelly auth password (401)." {
		t.Errorf("auth section = %+v", out.Sections[0])
	}
	if h := atomic.LoadInt32(&stub.hits); h != 1 {
		t.Errorf("device dialed %d times after 401, want 1 (no /settings follow-up)", h)
	}
}
