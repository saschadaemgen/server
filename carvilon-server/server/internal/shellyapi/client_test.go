package shellyapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rpcResult wraps a result payload in the JSON-RPC envelope.
func rpcResult(result string) string {
	return `{"id":1,"src":"shellypro4pm-08f9e0e5c790","result":` + result + `}`
}

// newRPCServer serves POST /rpc, dispatching on the method name.
func newRPCServer(t *testing.T, results map[string]string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rpc" {
			http.Error(w, "not the rpc endpoint", 404)
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res, ok := results[req.Method]
		if !ok {
			_, _ = w.Write([]byte(`{"id":1,"error":{"code":404,"message":"No handler for ` + req.Method + `"}}`))
			return
		}
		_, _ = w.Write([]byte(rpcResult(res)))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// addrOf strips the scheme from an httptest URL ("127.0.0.1:port").
func addrOf(ts *httptest.Server) string {
	return strings.TrimPrefix(ts.URL, "http://")
}

func TestGetDeviceInfo(t *testing.T) {
	ts := newRPCServer(t, map[string]string{
		"Shelly.GetDeviceInfo": `{"id":"shellypro4pm-08f9e0e5c790","name":"Growbox","model":"SPSW-104PE16EU","mac":"08F9E0E5C790","app":"Pro4PM","ver":"1.4.4","gen":2,"auth_en":false}`,
	})
	c := New(Options{Address: ts.URL}) // URL form must normalise to the bare host
	if got, want := c.Address(), addrOf(ts); got != want {
		t.Fatalf("Address() = %q, want %q", got, want)
	}
	di, err := c.GetDeviceInfo(context.Background())
	if err != nil {
		t.Fatalf("GetDeviceInfo: %v", err)
	}
	if di.DisplayName() != "Growbox" {
		t.Errorf("DisplayName = %q", di.DisplayName())
	}
	if di.ModelLabel() != "Shelly Pro4PM" {
		t.Errorf("ModelLabel = %q", di.ModelLabel())
	}
	if di.MACLabel() != "08F9E0E5C790" || di.FirmwareLabel() != "1.4.4" {
		t.Errorf("MAC/Firmware = %q / %q", di.MACLabel(), di.FirmwareLabel())
	}
	if di.AuthLabel() != "No" {
		t.Errorf("AuthLabel = %q", di.AuthLabel())
	}
}

// A null name falls back to the id; a missing app falls back to the
// model code; a fully drifted payload (array) degrades to empty labels
// instead of an error.
func TestGetDeviceInfoDegrades(t *testing.T) {
	ts := newRPCServer(t, map[string]string{
		"Shelly.GetDeviceInfo": `{"id":"shellypro4pm-08f9e0e5c790","name":null,"model":"SPSW-104PE16EU"}`,
	})
	c := New(Options{Address: addrOf(ts)})
	di, err := c.GetDeviceInfo(context.Background())
	if err != nil {
		t.Fatalf("GetDeviceInfo: %v", err)
	}
	if di.DisplayName() != "shellypro4pm-08f9e0e5c790" {
		t.Errorf("DisplayName fallback = %q", di.DisplayName())
	}
	if di.ModelLabel() != "SPSW-104PE16EU" {
		t.Errorf("ModelLabel fallback = %q", di.ModelLabel())
	}

	ts2 := newRPCServer(t, map[string]string{"Shelly.GetDeviceInfo": `[1,2,3]`})
	c2 := New(Options{Address: addrOf(ts2)})
	di2, err := c2.GetDeviceInfo(context.Background())
	if err != nil {
		t.Fatalf("drifted GetDeviceInfo: %v", err)
	}
	if di2.DisplayName() != "" || di2.ModelLabel() != "" {
		t.Errorf("drifted payload invented labels: %q / %q", di2.DisplayName(), di2.ModelLabel())
	}
}

func TestGetStatusComponents(t *testing.T) {
	ts := newRPCServer(t, map[string]string{
		"Shelly.GetStatus": `{
			"sys":{"uptime":12345},
			"switch:1":{"id":1,"output":false,"apower":0.0,"voltage":231.2,"current":0.0,"freq":50.0,"aenergy":{"total":812.406}},
			"switch:0":{"id":0,"output":true,"apower":52.3,"voltage":230.1,"current":0.229,"freq":50.0,"aenergy":{"total":32406.879}},
			"switch:2":"garbage-not-an-object",
			"switch:3":{"id":3,"aenergy":7},
			"switch:00":{"id":0,"output":false},
			"switch:+1":{"id":1,"output":true},
			"switch:9223372036854775807":{"id":0},
			"input:0":{"id":0,"state":false},
			"input:1":{"id":1,"percent":37.5},
			"wifi":{"sta_ip":"10.0.0.9"}
		}`,
	})
	c := New(Options{Address: addrOf(ts)})
	st, err := c.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	// The non-canonical hostile keys ("switch:00", "switch:+1", the
	// int64-range id) are ignored - no duplicate or absurd channels.
	if len(st.Switches) != 4 || len(st.Inputs) != 2 {
		t.Fatalf("components = %d switches / %d inputs, want 4 / 2", len(st.Switches), len(st.Inputs))
	}
	// sorted by id
	for i, sw := range st.Switches {
		if sw.ID != i {
			t.Errorf("switch order: index %d has id %d", i, sw.ID)
		}
	}
	sw0 := st.Switches[0]
	if sw0.StateLabel() != "On" || sw0.PowerLabel() != "52.3 W" || sw0.VoltageLabel() != "230.1 V" ||
		sw0.CurrentLabel() != "0.229 A" || sw0.FreqLabel() != "50.0 Hz" || sw0.EnergyLabel() != "32406.879 Wh" {
		t.Errorf("switch 0 labels: %q %q %q %q %q %q",
			sw0.StateLabel(), sw0.PowerLabel(), sw0.VoltageLabel(), sw0.CurrentLabel(), sw0.FreqLabel(), sw0.EnergyLabel())
	}
	if st.Switches[1].StateLabel() != "Off" {
		t.Errorf("switch 1 state = %q", st.Switches[1].StateLabel())
	}
	// the garbage component keeps its slot with empty labels
	if sw2 := st.Switches[2]; sw2.PowerLabel() != "" || sw2.StateLabel() != "" {
		t.Errorf("garbage switch invented values: %q / %q", sw2.PowerLabel(), sw2.StateLabel())
	}
	// aenergy as a bare number (not an object) degrades to no total
	if sw3 := st.Switches[3]; sw3.EnergyLabel() != "" {
		t.Errorf("non-object aenergy invented a total: %q", sw3.EnergyLabel())
	}
	if st.Inputs[0].StateLabel() != "Off" || st.Inputs[1].StateLabel() != "37.5 %" {
		t.Errorf("input labels: %q / %q", st.Inputs[0].StateLabel(), st.Inputs[1].StateLabel())
	}
}

func TestGetConfigNames(t *testing.T) {
	ts := newRPCServer(t, map[string]string{
		"Shelly.GetConfig": `{
			"switch:0":{"id":0,"name":"SANlight One"},
			"switch:1":{"id":1,"name":null},
			"input:0":{"id":0,"name":"Door contact"},
			"sys":{"device":{"name":"Growbox"}}
		}`,
	})
	c := New(Options{Address: addrOf(ts)})
	cfg, err := c.GetConfig(context.Background())
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if cfg.SwitchName(0) != "SANlight One" || cfg.SwitchName(1) != "" {
		t.Errorf("switch names: %q / %q", cfg.SwitchName(0), cfg.SwitchName(1))
	}
	if cfg.InputName(0) != "Door contact" {
		t.Errorf("input name: %q", cfg.InputName(0))
	}
	var nilCfg *Config
	if nilCfg.SwitchName(0) != "" {
		t.Errorf("nil Config not tolerated")
	}
}

// A result that is not a component map errors with the fixed
// redacted text - never the raw encoding/json error.
func TestStatusNonObjectResultRedacted(t *testing.T) {
	ts := newRPCServer(t, map[string]string{"Shelly.GetStatus": `[1,2,3]`, "Shelly.GetConfig": `"nope"`})
	c := New(Options{Address: addrOf(ts)})
	if _, err := c.GetStatus(context.Background()); err == nil || strings.Contains(err.Error(), "json") {
		t.Errorf("GetStatus err = %v, want fixed shellyapi text", err)
	}
	if _, err := c.GetConfig(context.Background()); err == nil || strings.Contains(err.Error(), "json") {
		t.Errorf("GetConfig err = %v, want fixed shellyapi text", err)
	}
}

// An RPC error frame maps to a coarse error; code 401 maps to
// ErrUnauthorized. Neither carries the foreign message text.
func TestRPCErrorFrame(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"error":{"code":401,"message":"secret-detail-do-not-leak"}}`))
	}))
	defer ts.Close()
	c := New(Options{Address: addrOf(ts)})
	_, err := c.GetStatus(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), "secret-detail") {
		t.Errorf("error leaked the rpc message: %v", err)
	}
}

// Transport failures and bad addresses never leak the device address
// into the error text (callers log these errors).
func TestErrorsNeverCarryTheAddress(t *testing.T) {
	// port 9 on localhost: connection refused (or a fast failure)
	c := New(Options{Address: "127.0.0.1:9"})
	_, err := c.GetStatus(context.Background())
	if err == nil {
		t.Skip("port 9 unexpectedly answered")
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), ":9") {
		t.Errorf("error text carries the address: %v", err)
	}
}

// The client never follows a redirect - a 3xx surfaces as its status.
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
	c := New(Options{Address: addrOf(ts)})
	_, err := c.GetStatus(context.Background())
	if err == nil || !strings.Contains(err.Error(), "http 302") {
		t.Fatalf("err = %v, want http 302", err)
	}
	if followed {
		t.Errorf("redirect was followed")
	}
}

// The RFC 7616 3.9.1 example, with its cnonce pinned: the produced
// Authorization header must carry the exact documented response hash.
func TestDigestRFC7616Example(t *testing.T) {
	old := randomCnonce
	randomCnonce = func() (string, error) { return "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ", nil }
	defer func() { randomCnonce = old }()

	challenge := `Digest realm="http-auth@example.org", qop="auth, auth-int", algorithm=SHA-256, nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`
	auth, err := digestAuthorization(challenge, "Mufasa", "Circle of Life", "GET", "/dir/index.html")
	if err != nil {
		t.Fatalf("digestAuthorization: %v", err)
	}
	for _, want := range []string{
		`response="753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"`,
		`username="Mufasa"`,
		`realm="http-auth@example.org"`,
		`uri="/dir/index.html"`,
		`algorithm=SHA-256`,
		`qop=auth`,
		`nc=00000001`,
		`opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`,
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization missing %s\n  got: %s", want, auth)
		}
	}
}

func TestDigestRejectsUnusable(t *testing.T) {
	for name, challenge := range map[string]string{
		"basic":         `Basic realm="x"`,
		"no nonce":      `Digest realm="x", algorithm=SHA-256`,
		"weird algo":    `Digest realm="x", nonce="n", algorithm=SHA-512-256`,
		"md5 downgrade": `Digest realm="x", nonce="n", algorithm=MD5`,
		"no algorithm":  `Digest realm="x", nonce="n"`,
		"auth-int only": `Digest realm="x", nonce="n", algorithm=SHA-256, qop="auth-int"`,
		"ctl in nonce":  "Digest realm=\"x\", nonce=\"n\r\nX-Evil: 1\", algorithm=SHA-256",
	} {
		if _, err := digestAuthorization(challenge, "admin", "pw", "POST", "/rpc"); err == nil {
			t.Errorf("%s: challenge accepted, want error", name)
		}
	}
}

// End-to-end 401 flow against a verifying digest server: first call
// unauthenticated, then exactly one retry whose response hash the
// server checks (SHA-256, qop=auth, Shelly's realm/nonce shape).
func TestDigestRetryAgainstVerifyingServer(t *testing.T) {
	const (
		realm = "shellypro4pm-08f9e0e5c790"
		nonce = "60dc32d6"
		pass  = "geheim-123"
	)
	h := func(parts ...string) string {
		sum := sha256.Sum256([]byte(strings.Join(parts, ":")))
		return hex.EncodeToString(sum[:])
	}
	attempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		authz := r.Header.Get("Authorization")
		if authz == "" {
			w.Header().Set("WWW-Authenticate",
				`Digest qop="auth", realm="`+realm+`", nonce="`+nonce+`", algorithm=SHA-256`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, params := splitChallenge(authz)
		ha1 := h("admin", realm, pass)
		ha2 := h(http.MethodPost, "/rpc")
		want := h(ha1, nonce, params["nc"], params["cnonce"], "auth", ha2)
		if params["response"] != want || params["username"] != "admin" || params["uri"] != "/rpc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(rpcResult(`{"switch:0":{"id":0,"output":true}}`)))
	}))
	defer ts.Close()

	c := New(Options{Address: addrOf(ts), Password: pass})
	st, err := c.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus with digest: %v", err)
	}
	if len(st.Switches) != 1 || st.Switches[0].StateLabel() != "On" {
		t.Errorf("authed status wrong: %+v", st)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (401 + one retry)", attempts)
	}

	// Wrong password: the retry fails and maps to ErrUnauthorized.
	attempts = 0
	cBad := New(Options{Address: addrOf(ts), Password: "falsch"})
	if _, err := cBad.GetStatus(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong password err = %v, want ErrUnauthorized", err)
	}
	if attempts != 2 {
		t.Errorf("wrong-password attempts = %d, want 2 (no retry loop)", attempts)
	}

	// No password configured: no retry at all.
	attempts = 0
	cNone := New(Options{Address: addrOf(ts)})
	if _, err := cNone.GetStatus(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("no password err = %v, want ErrUnauthorized", err)
	}
	if attempts != 1 {
		t.Errorf("no-password attempts = %d, want 1", attempts)
	}
}
