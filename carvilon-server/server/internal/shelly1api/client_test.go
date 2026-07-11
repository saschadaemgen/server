package shelly1api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// newGen1Server serves the Gen1 GET endpoints, dispatching on the URL path
// (the Gen1 API has no RPC envelope - every resource is its own path).
func newGen1Server(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.Error(w, "no such resource", 404)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// addrOf strips the scheme from an httptest URL ("127.0.0.1:port").
func addrOf(ts *httptest.Server) string {
	return strings.TrimPrefix(ts.URL, "http://")
}

// recordingServer captures every GET's path and query so a test can assert
// the exact request the write path sends (a Gen1 write IS its query
// string). Each request answers "{}".
type recordingServer struct {
	ts   *httptest.Server
	mu   sync.Mutex
	reqs []recordedGet
}

type recordedGet struct {
	Path  string
	Query url.Values
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.reqs = append(rs.reqs, recordedGet{Path: r.URL.Path, Query: r.URL.Query()})
		rs.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(rs.ts.Close)
	return rs
}

func (rs *recordingServer) last() recordedGet {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.reqs[len(rs.reqs)-1]
}

func (rs *recordingServer) count() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.reqs)
}

func (rs *recordingServer) client() *Client {
	return New(Options{Address: addrOf(rs.ts)})
}

func TestGetIdentityGen1(t *testing.T) {
	ts := newGen1Server(t, map[string]string{
		"/shelly": `{"type":"SHSW-25","mac":"E8DB84D1B2C3","auth":false,"fw":"20230913-112003/v1.14.0-gcb84623","longid":1}`,
	})
	c := New(Options{Address: ts.URL + "/"}) // pasted URL form must normalise to the bare host
	if got, want := c.Address(), addrOf(ts); got != want {
		t.Fatalf("Address() = %q, want %q", got, want)
	}
	ident, err := c.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	// A missing "gen" field is the documented Gen1 signature.
	if ident.Generation() != 1 {
		t.Errorf("Generation = %d, want 1", ident.Generation())
	}
	if ident.TypeLabel() != "SHSW-25" {
		t.Errorf("TypeLabel = %q", ident.TypeLabel())
	}
	if ident.MACLabel() != "E8DB84D1B2C3" {
		t.Errorf("MACLabel = %q", ident.MACLabel())
	}
	if ident.FirmwareLabel() != "20230913-112003/v1.14.0-gcb84623" {
		t.Errorf("FirmwareLabel = %q", ident.FirmwareLabel())
	}
	if ident.AuthLabel() != "No" {
		t.Errorf("AuthLabel = %q", ident.AuthLabel())
	}
}

// A /shelly answer WITH a "gen" field is a Gen2+ device that must be
// classified as such (the probe doubles as the generation classifier);
// the labels fall back to the Gen2 field names.
func TestGetIdentityGen2(t *testing.T) {
	ts := newGen1Server(t, map[string]string{
		"/shelly": `{"id":"shellypro4pm-08f9e0e5c790","gen":2,"model":"SPSW-104PE16EU","app":"Pro4PM","mac":"08F9E0E5C790","auth_en":true,"fw_id":"1.4.4"}`,
	})
	c := New(Options{Address: addrOf(ts)})
	ident, err := c.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	if ident.Generation() != 2 {
		t.Errorf("Generation = %d, want 2", ident.Generation())
	}
	if ident.TypeLabel() != "SPSW-104PE16EU" {
		t.Errorf("TypeLabel fallback = %q", ident.TypeLabel())
	}
	if ident.MACLabel() != "08F9E0E5C790" || ident.FirmwareLabel() != "1.4.4" {
		t.Errorf("MAC/Firmware = %q / %q", ident.MACLabel(), ident.FirmwareLabel())
	}
	if ident.AuthLabel() != "Yes" {
		t.Errorf("AuthLabel = %q", ident.AuthLabel())
	}
}

// A present but garbage "gen" must classify as 0 (unknown) - callers must
// not guess a generation from an answer that is too odd to trust.
func TestGetIdentityGarbageGen(t *testing.T) {
	for name, body := range map[string]string{
		"non-numeric": `{"gen":"garbage","mac":"AABBCC"}`,
		"negative":    `{"gen":-3,"mac":"AABBCC"}`,
	} {
		ts := newGen1Server(t, map[string]string{"/shelly": body})
		c := New(Options{Address: addrOf(ts)})
		ident, err := c.GetIdentity(context.Background())
		if err != nil {
			t.Fatalf("%s: GetIdentity: %v", name, err)
		}
		if ident.Generation() != 0 {
			t.Errorf("%s: Generation = %d, want 0", name, ident.Generation())
		}
	}
}

// Gen1 auth is HTTP Basic: with a password configured the header is sent
// on every request (no challenge round-trip), the username defaults to
// "admin" but stays configurable; without a password no header is sent.
func TestBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var gotHeader bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotHeader = r.BasicAuth()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := New(Options{Address: addrOf(ts), Password: "pw-123"})
	if _, err := c.GetStatus(context.Background()); err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !gotHeader || gotUser != "admin" || gotPass != "pw-123" {
		t.Errorf("default-user auth = %v %q:%q, want admin:pw-123", gotHeader, gotUser, gotPass)
	}

	cCustom := New(Options{Address: addrOf(ts), Username: "service", Password: "pw-123"})
	if _, err := cCustom.GetStatus(context.Background()); err != nil {
		t.Fatalf("GetStatus custom user: %v", err)
	}
	if !gotHeader || gotUser != "service" {
		t.Errorf("custom-user auth = %v %q, want service", gotHeader, gotUser)
	}

	cNone := New(Options{Address: addrOf(ts)})
	if _, err := cNone.GetStatus(context.Background()); err != nil {
		t.Fatalf("GetStatus without password: %v", err)
	}
	if gotHeader {
		t.Errorf("Authorization sent without a configured password")
	}
}

// 401 and 403 both map to the fixed ErrUnauthorized sentinel (the UI shows
// one message for "check the auth password").
func TestUnauthorizedMapsToSentinel(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c := New(Options{Address: addrOf(ts), Password: "wrong"})
		_, err := c.GetStatus(context.Background())
		ts.Close()
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("http %d: err = %v, want ErrUnauthorized", code, err)
		}
	}
}

// Transport failures never leak the device address into the error text
// (callers log these errors - the redaction contract kept from shellyapi).
func TestErrorsNeverCarryTheAddress(t *testing.T) {
	// Bind an ephemeral port and close it again: dialing it now refuses.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	c := New(Options{Address: addr})
	_, err = c.GetStatus(context.Background())
	if err == nil {
		t.Skip("freed port unexpectedly answered")
	}
	port := addr[strings.LastIndexByte(addr, ':'):]
	if strings.Contains(err.Error(), addr) || strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), port) {
		t.Errorf("error text carries the address: %v", err)
	}
}

// The client never follows a redirect - a 3xx surfaces as its status, and
// the Basic credential is never replayed to the redirect target.
func TestRedirectsAreNotFollowed(t *testing.T) {
	followed := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer ts.Close()
	c := New(Options{Address: addrOf(ts), Password: "pw-123"})
	_, err := c.GetStatus(context.Background())
	if err == nil || !strings.Contains(err.Error(), "http 302") {
		t.Fatalf("err = %v, want http 302", err)
	}
	if followed {
		t.Errorf("redirect was followed")
	}
}

func TestGetStatus(t *testing.T) {
	ts := newGen1Server(t, map[string]string{
		"/status": `{
			"relays":[{"ison":true,"overpower":false,"source":"http"},{"ison":false,"source":"input"}],
			"meters":[{"power":52.3,"is_valid":true,"total":600},{}],
			"inputs":[{"input":1,"event":"S","event_cnt":2}],
			"tmp":{"tC":41.2,"is_valid":true},
			"uptime":12345,
			"has_update":false,
			"mqtt":{"connected":true}
		}`,
	})
	c := New(Options{Address: addrOf(ts)})
	st, err := c.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if len(st.Relays) != 2 || len(st.Meters) != 2 || len(st.Inputs) != 1 {
		t.Fatalf("arrays = %d relays / %d meters / %d inputs, want 2 / 2 / 1",
			len(st.Relays), len(st.Meters), len(st.Inputs))
	}
	if st.Relays[0].StateLabel() != "On" || st.Relays[1].StateLabel() != "Off" {
		t.Errorf("relay states: %q / %q", st.Relays[0].StateLabel(), st.Relays[1].StateLabel())
	}
	if st.Meters[0].PowerLabel() != "52.3 W" {
		t.Errorf("PowerLabel = %q", st.Meters[0].PowerLabel())
	}
	// The device counts WATT-MINUTES; the label must render Wh (600 / 60).
	if st.Meters[0].EnergyLabel() != "10 Wh" {
		t.Errorf("EnergyLabel = %q, want %q", st.Meters[0].EnergyLabel(), "10 Wh")
	}
	// An empty meter entry degrades to "-" instead of inventing readings.
	if st.Meters[1].PowerLabel() != "-" || st.Meters[1].EnergyLabel() != "-" {
		t.Errorf("empty meter labels: %q / %q", st.Meters[1].PowerLabel(), st.Meters[1].EnergyLabel())
	}
	if st.TempLabel() != "41.2 °C" {
		t.Errorf("TempLabel = %q", st.TempLabel())
	}
	if v, ok := st.Inputs[0].Input.Bool(); !ok || !v {
		t.Errorf("input state = %v/%v, want true", v, ok)
	}
	if st.Inputs[0].Event.String() != "S" {
		t.Errorf("input event = %q", st.Inputs[0].Event.String())
	}
	if v, ok := st.MQTT.Connected.Bool(); !ok || !v {
		t.Errorf("mqtt connected = %v/%v, want true", v, ok)
	}
}

func TestGetSettings(t *testing.T) {
	ts := newGen1Server(t, map[string]string{
		"/settings": `{
			"device":{"type":"SHSW-25","mac":"E8DB84D1B2C3","hostname":"shellyswitch25-D1B2C3"},
			"name":"Hallway",
			"mode":"relay",
			"fw":"20230913-112003/v1.14.0-gcb84623",
			"relays":[
				{"name":"Ceiling light","appliance_type":"General","default_state":"off",
				 "auto_on":0,"auto_off":300,"schedule":true,
				 "schedule_rules":["0700-0123456-on","2200-0123456-off"]},
				{"name":null,"schedule":false,"schedule_rules":[]}
			],
			"mqtt":{"enable":true,"server":"192.0.2.10:1883","user":"shelly-d1b2c3","id":"shelly-d1b2c3","retain":true,"max_qos":1,"keep_alive":60,"clean_session":true,"update_period":30},
			"login":{"enabled":true,"unprotected":false,"username":"admin"},
			"cloud":{"enabled":false},
			"coiot":{"update_period":15}
		}`,
	})
	c := New(Options{Address: addrOf(ts)})
	st, err := c.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.Device.Type.String() != "SHSW-25" || st.Name.String() != "Hallway" || st.Mode.String() != "relay" {
		t.Errorf("device/name/mode: %q / %q / %q", st.Device.Type.String(), st.Name.String(), st.Mode.String())
	}
	if len(st.Relays) != 2 {
		t.Fatalf("relays = %d, want 2", len(st.Relays))
	}
	r0 := st.Relays[0]
	if r0.Name.String() != "Ceiling light" || r0.AutoOff.String() != "300" {
		t.Errorf("relay 0 name/auto_off: %q / %q", r0.Name.String(), r0.AutoOff.String())
	}
	if v, ok := r0.Schedule.Bool(); !ok || !v {
		t.Errorf("relay 0 schedule = %v/%v, want true", v, ok)
	}
	if want := []string{"0700-0123456-on", "2200-0123456-off"}; !reflect.DeepEqual(r0.ScheduleRules, want) {
		t.Errorf("schedule_rules = %v, want %v", r0.ScheduleRules, want)
	}
	if !st.Relays[1].Name.Empty() || len(st.Relays[1].ScheduleRules) != 0 {
		t.Errorf("relay 1 should have no name and no rules: %q / %v",
			st.Relays[1].Name.String(), st.Relays[1].ScheduleRules)
	}
	// The mqtt read-back nests with UNPREFIXED keys (the write side uses
	// mqtt_-prefixed query params - asymmetric by design of the frozen API).
	if v, ok := st.MQTT.Enable.Bool(); !ok || !v {
		t.Errorf("mqtt enable = %v/%v, want true", v, ok)
	}
	if st.MQTT.Server.String() != "192.0.2.10:1883" || st.MQTT.User.String() != "shelly-d1b2c3" ||
		st.MQTT.ID.String() != "shelly-d1b2c3" || st.MQTT.MaxQoS.String() != "1" {
		t.Errorf("mqtt read-back: server %q user %q id %q max_qos %q",
			st.MQTT.Server.String(), st.MQTT.User.String(), st.MQTT.ID.String(), st.MQTT.MaxQoS.String())
	}
	if st.Login.Username.String() != "admin" {
		t.Errorf("login username = %q", st.Login.Username.String())
	}
	// Raw must preserve the whole payload - keys the typed struct does not
	// model (coiot) are rendered from Raw by the coverage-driven UI.
	if !strings.Contains(string(st.Raw), `"coiot"`) {
		t.Errorf("Raw lost untyped keys: %s", st.Raw)
	}
}

// The Gen1 MQTT provisioning write is a GET /settings whose query carries
// the mqtt_-prefixed keys - assert the exact wire form.
func TestSetMQTTConfig(t *testing.T) {
	rs := newRecordingServer(t)
	err := rs.client().SetMQTTConfig(context.Background(), MQTTProvision{
		Server: "192.0.2.10:1883", User: "shelly-d1b2c3", Pass: "broker-secret",
		ID: "shelly-d1b2c3", Retain: true, MaxQoS: 1,
	})
	if err != nil {
		t.Fatalf("SetMQTTConfig: %v", err)
	}
	req := rs.last()
	if req.Path != "/settings" {
		t.Fatalf("path = %q, want /settings", req.Path)
	}
	want := map[string]string{
		"mqtt_enable":  "1",
		"mqtt_server":  "192.0.2.10:1883",
		"mqtt_user":    "shelly-d1b2c3",
		"mqtt_pass":    "broker-secret",
		"mqtt_id":      "shelly-d1b2c3",
		"mqtt_retain":  "1",
		"mqtt_max_qos": "1",
	}
	for k, v := range want {
		if got := req.Query.Get(k); got != v {
			t.Errorf("query[%q] = %q, want %q", k, got, v)
		}
	}
}

func TestSetRelay(t *testing.T) {
	rs := newRecordingServer(t)
	if err := rs.client().SetRelay(context.Background(), 2, true); err != nil {
		t.Fatalf("SetRelay on: %v", err)
	}
	if req := rs.last(); req.Path != "/relay/2" || req.Query.Get("turn") != "on" {
		t.Errorf("on request = %q?%s, want /relay/2?turn=on", req.Path, req.Query.Encode())
	}
	if err := rs.client().SetRelay(context.Background(), 0, false); err != nil {
		t.Fatalf("SetRelay off: %v", err)
	}
	if req := rs.last(); req.Path != "/relay/0" || req.Query.Get("turn") != "off" {
		t.Errorf("off request = %q?%s, want /relay/0?turn=off", req.Path, req.Query.Encode())
	}
	// Out-of-range channels error locally - no request may reach the device.
	before := rs.count()
	for _, ch := range []int{-1, 8} {
		if err := rs.client().SetRelay(context.Background(), ch, true); err == nil {
			t.Errorf("channel %d accepted, want error", ch)
		}
	}
	if rs.count() != before {
		t.Errorf("out-of-range channel produced a request")
	}
}

// The on-device schedule is written as one whole set: rules joined with
// the documented comma, an empty list clears the schedule.
func TestSetScheduleRules(t *testing.T) {
	rs := newRecordingServer(t)
	rules := []string{"0700-0123456-on", "2200-0123456-off"}
	if err := rs.client().SetScheduleRules(context.Background(), 1, true, rules); err != nil {
		t.Fatalf("SetScheduleRules: %v", err)
	}
	req := rs.last()
	if req.Path != "/settings/relay/1" {
		t.Fatalf("path = %q, want /settings/relay/1", req.Path)
	}
	if req.Query.Get("schedule") != "1" {
		t.Errorf("schedule = %q, want 1", req.Query.Get("schedule"))
	}
	if got := req.Query.Get("schedule_rules"); got != "0700-0123456-on,2200-0123456-off" {
		t.Errorf("schedule_rules = %q", got)
	}

	// Clearing: the schedule_rules key must still be SENT (empty), or the
	// device would keep the old set.
	if err := rs.client().SetScheduleRules(context.Background(), 0, false, nil); err != nil {
		t.Fatalf("SetScheduleRules clear: %v", err)
	}
	req = rs.last()
	if req.Query.Get("schedule") != "0" {
		t.Errorf("clear schedule = %q, want 0", req.Query.Get("schedule"))
	}
	if _, ok := req.Query["schedule_rules"]; !ok {
		t.Errorf("clear did not send the schedule_rules key")
	}
	if got := req.Query.Get("schedule_rules"); got != "" {
		t.Errorf("clear schedule_rules = %q, want empty", got)
	}
	// Bounds are guarded here too.
	if err := rs.client().SetScheduleRules(context.Background(), 8, true, rules); err == nil {
		t.Errorf("channel 8 accepted, want error")
	}
}

func TestSetLogin(t *testing.T) {
	rs := newRecordingServer(t)
	if err := rs.client().SetLogin(context.Background(), true, "admin", "pw-123"); err != nil {
		t.Fatalf("SetLogin: %v", err)
	}
	req := rs.last()
	if req.Path != "/settings/login" {
		t.Fatalf("path = %q, want /settings/login", req.Path)
	}
	if req.Query.Get("enabled") != "1" || req.Query.Get("username") != "admin" || req.Query.Get("password") != "pw-123" {
		t.Errorf("login query = %s", req.Query.Encode())
	}
	// Disabling without a credential omits the username/password keys
	// entirely (an empty password param could clobber the stored one).
	if err := rs.client().SetLogin(context.Background(), false, "", ""); err != nil {
		t.Fatalf("SetLogin disable: %v", err)
	}
	req = rs.last()
	if req.Query.Get("enabled") != "0" {
		t.Errorf("disable enabled = %q, want 0", req.Query.Get("enabled"))
	}
	for _, k := range []string{"username", "password"} {
		if _, ok := req.Query[k]; ok {
			t.Errorf("disable sent the %s key", k)
		}
	}
}

// TestLightShapes decodes the light-class settings/status shapes measured
// on a real SHRGBW2 (fw v1.14.0, color mode) and pins the RSSI label.
func TestLightShapes(t *testing.T) {
	ts := newGen1Server(t, map[string]string{
		"/settings": `{"device":{"type":"SHRGBW2"},"mode":"color","alt_modes":["white"],"discoverable":false,
			"lights":[{"name":"strip","ison":true,"red":255,"green":120,"blue":40,"white":0,"gain":80,
			           "transition":0,"effect":0,"default_state":"switch","schedule":false,"schedule_rules":[]}]}`,
		"/status": `{"lights":[{"ison":true,"mode":"color","red":255,"green":120,"blue":40,"white":0,"gain":80,"power":9.6}],
			"meters":[{"power":9.6,"total":600}],"wifi_sta":{"rssi":-94},
			"update":{"status":"idle","has_update":true,"new_version":"v1.14.1"}}`,
	})
	c := New(Options{Address: addrOf(ts)})
	sett, err := c.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if len(sett.Lights) != 1 || sett.Lights[0].Name.String() != "strip" {
		t.Fatalf("lights = %+v", sett.Lights)
	}
	if got := sett.Lights[0].DefaultState.String(); got != "switch" {
		t.Errorf("default_state = %q", got)
	}
	if v, ok := sett.Discoverable.Bool(); !ok || v {
		t.Errorf("discoverable = %v/%v, want false", v, ok)
	}
	if len(sett.AltModes) != 1 || sett.AltModes[0] != "white" {
		t.Errorf("alt_modes = %v", sett.AltModes)
	}
	st, err := c.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if len(st.Lights) != 1 || st.Lights[0].StateLabel() != "On" || st.Lights[0].PowerLabel() != "9.6 W" {
		t.Fatalf("light status = %+v", st.Lights)
	}
	if got := st.RSSILabel(); got != "-94 dBm" {
		t.Errorf("rssi = %q", got)
	}
	if v, ok := st.Update.HasUpdate.Bool(); !ok || !v {
		t.Errorf("update.has_update = %v/%v", v, ok)
	}
}

// TestSetLight pins the control transport: mode dispatch into the URL
// path, params passed through, and the mode/channel guards failing
// locally (no request ever leaves for a bad mode).
func TestSetLight(t *testing.T) {
	rs := newRecordingServer(t)
	c := rs.client()
	if err := c.SetLight(context.Background(), "color", 0, url.Values{"turn": {"on"}, "red": {"255"}}); err != nil {
		t.Fatalf("SetLight: %v", err)
	}
	last := rs.last()
	if last.Path != "/color/0" || last.Query.Get("turn") != "on" || last.Query.Get("red") != "255" {
		t.Errorf("request = %s %v", last.Path, last.Query)
	}
	if err := c.SetLightSettings(context.Background(), "white", 3, url.Values{"transition": {"500"}}); err != nil {
		t.Fatalf("SetLightSettings: %v", err)
	}
	if last := rs.last(); last.Path != "/settings/white/3" || last.Query.Get("transition") != "500" {
		t.Errorf("settings request = %s %v", last.Path, last.Query)
	}
	if err := c.SetLightScheduleRules(context.Background(), "color", 0, true, []string{"0700-0123456-on"}); err != nil {
		t.Fatalf("SetLightScheduleRules: %v", err)
	}
	if last := rs.last(); last.Path != "/settings/color/0" || last.Query.Get("schedule_rules") != "0700-0123456-on" {
		t.Errorf("schedule request = %s %v", last.Path, last.Query)
	}
	before := rs.count()
	if err := c.SetLight(context.Background(), "settings", 0, url.Values{}); err == nil {
		t.Error("bogus mode accepted")
	}
	if err := c.SetLight(context.Background(), "color", 9, url.Values{}); err == nil {
		t.Error("out-of-range channel accepted")
	}
	if rs.count() != before {
		t.Error("guarded calls still sent requests")
	}
}
