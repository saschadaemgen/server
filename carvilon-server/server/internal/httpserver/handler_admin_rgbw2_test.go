package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/shelly1api"
	"carvilon.local/server/internal/shellystore"
)

// rgbw2Stub is a fake SHRGBW2 in color mode, shaped after the answers a
// real device gave (fw v1.14.0): "discoverable": false, a light with a
// name and colour state, per-light power, the shared watt-minute meter,
// and a -94 dBm WiFi signal. Every request is recorded so tests can pin
// the exact control/settings writes.
type rgbw2Stub struct {
	ts   *httptest.Server
	mu   sync.Mutex
	reqs []string
}

func (s *rgbw2Stub) addr() string { return strings.TrimPrefix(s.ts.URL, "http://") }

func (s *rgbw2Stub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func newRGBW2Stub(t *testing.T) *rgbw2Stub {
	t.Helper()
	s := &rgbw2Stub{}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.URL.String())
		s.mu.Unlock()
		switch {
		case r.URL.Path == "/shelly":
			_, _ = w.Write([]byte(`{"type":"SHRGBW2","mac":"A4CF12C0FFEE","auth":false,"fw":"20230913-114340/v1.14.0-gcb84623","longid":1,"discoverable":false}`))
		case r.URL.Path == "/status":
			_, _ = w.Write([]byte(`{
				"lights":[{"ison":true,"mode":"color","red":255,"green":120,"blue":40,"white":0,"gain":80,"effect":0,"transition":0,"power":9.6,"overpower":false}],
				"meters":[{"power":9.6,"is_valid":true,"total":600}],
				"inputs":[{"input":0}],
				"mqtt":{"connected":false},
				"update":{"status":"idle","has_update":false},
				"wifi_sta":{"rssi":-94}
			}`))
		case r.URL.Path == "/settings":
			_, _ = w.Write([]byte(`{
				"device":{"type":"SHRGBW2","mac":"A4CF12C0FFEE"},
				"name":"colorbox",
				"mode":"color",
				"alt_modes":["white"],
				"discoverable":false,
				"fw":"20230913-114340/v1.14.0-gcb84623",
				"lights":[{"name":"strip","ison":true,"red":255,"green":120,"blue":40,"white":0,"gain":80,"transition":0,"effect":0,
				           "default_state":"switch","auto_on":0,"auto_off":0,"btn_type":"toggle","btn_reverse":0,
				           "schedule":false,"schedule_rules":[]}],
				"mqtt":{"enable":true,"server":"192.0.2.65:1883","user":"olduser","id":"old-custom-id","update_period":30},
				"login":{"enabled":false,"username":"admin"},
				"cloud":{"enabled":false}
			}`))
		default:
			// control/settings writes just echo ok - the recorded URL is
			// the assertion surface
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// adoptRGBW2 seeds the store with the stub as an adopted, classified
// Gen1 light and points the fleet at it (the manual-address path a
// non-discoverable device must take).
func adoptRGBW2(t *testing.T, env *testEnv, stub *rgbw2Stub) int64 {
	t.Helper()
	ctx := context.Background()
	if err := env.platformCfg.Set(ctx, platformconfig.KeyShellyEnabled, "1"); err != nil {
		t.Fatalf("enable shelly: %v", err)
	}
	store := env.srv.shellystore
	if err := store.ReplaceManual(ctx, []string{stub.addr()}); err != nil {
		t.Fatalf("adopt manual: %v", err)
	}
	active, err := store.ListActive(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("list active: %v (%d rows)", err, len(active))
	}
	id := active[0].ID
	if err := store.SetIdentity(ctx, id, "A4CF12C0FFEE", "SHRGBW2", shellystore.Gen1); err != nil {
		t.Fatalf("set identity: %v", err)
	}
	env.srv.SetShellyClients([]ShellyDeviceClient{{
		StoreID: id,
		Gen:     shellystore.Gen1,
		Gen1:    shelly1api.New(shelly1api.Options{Address: stub.addr()}),
	}})
	return id
}

// The RGBW2 renders as a LIGHT row (its own category, not a switch) with
// the light-kind channel vocabulary the cockpit's card dispatch reads.
func TestAdminUA_RGBW2Row(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	wireUA(t, env, newUAStub(t))
	stub := newRGBW2Stub(t)
	adoptRGBW2(t, env, stub)

	body := getBody(t, env, "/a/devices")
	// the row div spans several template lines; join the block around the
	// data-kind line so every attribute is in one haystack
	lines := strings.Split(body, "\n")
	row := ""
	for i, line := range lines {
		if strings.Contains(line, `data-kind="shelly"`) {
			lo := i - 6
			if lo < 0 {
				lo = 0
			}
			row = strings.Join(lines[lo:i+1], "\n")
			break
		}
	}
	if row == "" {
		t.Fatal("no shelly row rendered")
	}
	for _, want := range []string{
		`data-cat="rgbw"`, `data-gen="1"`,
		`data-prefix="shellies/shelly-a4cf12c0ffee"`,
		"&#34;kind&#34;:&#34;color&#34;",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("row lacks %q:\n%s", want, row)
		}
	}
	if !strings.Contains(body, "Shelly RGBW2") {
		t.Error("model label 'Shelly RGBW2' missing")
	}
	if !strings.Contains(body, "RGBW Dimmer") {
		t.Error("RGBW Dimmer group/facet label missing")
	}
}

// The lazy detail serves the light shape: state + per-light power +
// colour + the shared meter's watt-minute energy, plus the honest device
// rows (weak WiFi surfaced, mDNS-off called out).
func TestAdminUA_RGBW2Detail(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	wireUA(t, env, newUAStub(t))
	stub := newRGBW2Stub(t)
	adoptRGBW2(t, env, stub)

	body := getBody(t, env, "/a/devices/shelly/"+url.PathEscape(stub.addr()))
	var out struct {
		Sections []struct {
			Title string  `json:"title"`
			Rows  []kvRow `json:"rows"`
		} `json:"sections"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("detail not JSON: %v (%s)", err, body)
	}
	find := func(title, key string) string {
		for _, s := range out.Sections {
			if s.Title != title {
				continue
			}
			for _, r := range s.Rows {
				if r.Key == key {
					return r.Value
				}
			}
		}
		return ""
	}
	if got := find("Light 1 · strip", "State"); got != "On" {
		t.Errorf("light state = %q, want On", got)
	}
	if got := find("Light 1 · strip", "Power"); got != "9.6 W" {
		t.Errorf("light power = %q, want 9.6 W", got)
	}
	if got := find("Light 1 · strip", "Color (R G B W)"); got != "255 120 40 0" {
		t.Errorf("light color = %q", got)
	}
	if got := find("Light 1 · strip", "Energy"); got != "10 Wh" {
		t.Errorf("energy = %q, want 10 Wh (600 watt-minutes)", got)
	}
	if got := find("Device", "WiFi signal"); got != "-94 dBm" {
		t.Errorf("rssi = %q, want -94 dBm", got)
	}
	if got := find("Device", "mDNS announce"); !strings.Contains(got, "Off") {
		t.Errorf("mDNS-off note missing, got %q", got)
	}
}

// The light control endpoint drives /color/{ch} with clamped params and
// only the provided keys; the channel-settings and schedule endpoints
// route onto the light settings path (never /settings/relay/...).
func TestDesignerShelly1Light(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newRGBW2Stub(t)
	id := adoptRGBW2(t, env, stub)
	base := "/a/designer/shelly/" + itoa64(id)

	post := func(path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	lastReq := func(prefix string) string {
		t.Helper()
		for _, u := range stub.requests() {
			if strings.HasPrefix(u, prefix) {
				return u
			}
		}
		t.Fatalf("stub never saw a %s request; got %v", prefix, stub.requests())
		return ""
	}

	// on + colour, gain clamped down from 900 to 100
	if resp := post(base+"/gen1/light/0", `{"mode":"color","on":true,"red":255,"gain":900}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("light control = %d", resp.StatusCode)
	}
	u, _ := url.Parse(lastReq("/color/0"))
	q := u.Query()
	if q.Get("turn") != "on" || q.Get("red") != "255" || q.Get("gain") != "100" {
		t.Errorf("control query = %v", q)
	}
	if q.Get("green") != "" {
		t.Error("unprovided key was sent - a gain nudge must not re-send stale colour")
	}
	// a bogus mode never reaches the device
	if resp := post(base+"/gen1/light/0", `{"mode":"../settings","on":true}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bogus mode = %d, want 400", resp.StatusCode)
	}

	// channel settings route to the light path with the light whitelist
	if resp := post(base+"/gen1/channel/0/settings", `{"config":{"transition":500,"max_power":100}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("light settings = %d", resp.StatusCode)
	}
	u, _ = url.Parse(lastReq("/settings/color/0"))
	if u.Query().Get("transition") != "500" {
		t.Errorf("settings query = %v", u.Query())
	}
	if u.Query().Get("max_power") != "" {
		t.Error("relay-only key leaked through the light whitelist")
	}

	// the schedule write lands on the light path as a whole set
	if resp := post(base+"/gen1/channel/0/schedule", `{"enabled":true,"rules":["0700-0123456-on"]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("light schedule = %d", resp.StatusCode)
	}
	found := false
	for _, u := range stub.requests() {
		if strings.HasPrefix(u, "/settings/color/0") && strings.Contains(u, "schedule_rules=0700-0123456-on") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("schedule write missing from stub log: %v", stub.requests())
	}
}

// The channel GET serves the light shape with the slider-seeding state.
func TestDesignerShelly1LightChannel(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newRGBW2Stub(t)
	id := adoptRGBW2(t, env, stub)

	body := getBody(t, env, "/a/designer/shelly/"+itoa64(id)+"/gen1/channel/0")
	var out struct {
		Kind  string         `json:"kind"`
		Light map[string]any `json:"light"`
		State map[string]any `json:"state"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("channel not JSON: %v (%s)", err, body)
	}
	if out.Kind != "color" {
		t.Errorf("kind = %q, want color", out.Kind)
	}
	if out.Light["name"] != "strip" || out.Light["default_state"] != "switch" {
		t.Errorf("light shape = %v", out.Light)
	}
	if out.State["red"] != "255" || out.State["gain"] != "80" || out.State["ison"] != true {
		t.Errorf("seed state = %v", out.State)
	}
}

func itoa64(id int64) string { return strconv.FormatInt(id, 10) }
