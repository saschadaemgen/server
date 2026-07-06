package protectapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The Integration API authenticates with X-API-KEY - NOT a Bearer
// token. Assert the exact header and that no Authorization leaks.
func TestClient_SendsXAPIKeyHeader(t *testing.T) {
	var gotKey, gotAuth, gotAccept string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-KEY")
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"applicationVersion":"5.0.34"}`))
	}))
	defer ts.Close()

	c := New(Options{BaseURL: ts.URL, APIKey: "test-key"})
	mi, err := c.GetMetaInfo(context.Background())
	if err != nil {
		t.Fatalf("GetMetaInfo: %v", err)
	}
	if gotKey != "test-key" {
		t.Errorf("X-API-KEY = %q, want %q", gotKey, "test-key")
	}
	if gotAuth != "" {
		t.Errorf("Authorization header sent (%q), want none", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if mi.ApplicationVersion.String() != "5.0.34" {
		t.Errorf("version = %q", mi.ApplicationVersion.String())
	}
}

// Requests hit the /proxy/protect/integration prefix, and a pasted
// full Integration base URL normalises back to the controller root.
func TestClient_PathAndBaseURLNormalisation(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`[]`))
	}))
	defer ts.Close()

	for _, base := range []string{
		ts.URL,
		ts.URL + "/",
		ts.URL + "/proxy/protect/integration",
		ts.URL + "/proxy/protect/integration/v1",
		ts.URL + "/proxy/protect/integration/v1/",
	} {
		c := New(Options{BaseURL: base, APIKey: "k"})
		if _, err := c.ListCameras(context.Background()); err != nil {
			t.Fatalf("ListCameras (base %q): %v", base, err)
		}
		if gotPath != "/proxy/protect/integration/v1/cameras" {
			t.Errorf("path = %q for base %q", gotPath, base)
		}
	}
}

// Status codes map onto the sentinel errors; other failures carry the
// bare status code and never the host.
func TestClient_ErrorMapping(t *testing.T) {
	status := 200
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, APIKey: "k"})

	status = 401
	if _, err := c.ListCameras(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("401 -> %v, want ErrUnauthorized", err)
	}
	status = 403
	if _, err := c.ListCameras(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("403 -> %v, want ErrUnauthorized", err)
	}
	status = 404
	if _, err := c.ListSensors(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Errorf("404 -> %v, want ErrNotFound", err)
	}
	status = 500
	_, err := c.ListCameras(context.Background())
	if err == nil {
		t.Fatalf("500 -> nil error")
	}
	if strings.Contains(err.Error(), ts.URL) {
		t.Errorf("error leaks the host: %v", err)
	}
}

// Transport errors (unreachable host, timeout) must not leak the
// configured host or dial address - these errors get logged.
func TestClient_TransportErrorRedactsHost(t *testing.T) {
	// A closed port on localhost: the raw url.Error/dial error would
	// carry "127.0.0.1:9" - the redacted error must not.
	c := New(Options{BaseURL: "http://127.0.0.1:9", APIKey: "k", Timeout: 2 * time.Second})
	_, err := c.ListCameras(context.Background())
	if err == nil {
		t.Fatalf("want transport error")
	}
	for _, forbidden := range []string{"127.0.0.1", ":9", "http://"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("transport error leaks %q: %v", forbidden, err)
		}
	}
}

// The cameras decode survives drifted items: a non-string id falls
// back to the raw map, unknown fields are ignored, and an item with
// no id at all is dropped without killing its siblings.
func TestListCameras_TolerantDecode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id":"cam-1","name":"Front","state":"CONNECTED","mac":"AA:BB:CC:00:11:22","videoMode":"default","hdrType":"auto","hasPackageCamera":false},
			{"id":12345,"name":"Drifted","state":"DISCONNECTED"},
			{"name":"no id at all"},
			{"id":"cam-2","state":"DISCONNECTED"}
		]`))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, APIKey: "k"})

	cams, err := c.ListCameras(context.Background())
	if err != nil {
		t.Fatalf("ListCameras: %v", err)
	}
	if len(cams) != 3 {
		t.Fatalf("got %d cameras, want 3 (the id-less item drops)", len(cams))
	}
	if !cams[0].IsOnline() || cams[0].MACLabel() != "AA:BB:CC:00:11:22" {
		t.Errorf("cam-1 decoded wrong: %+v", cams[0])
	}
	if cams[0].VideoModeLabel() != "default" || cams[0].HDRTypeLabel() != "auto" || cams[0].PackageCameraLabel() != "No" {
		t.Errorf("cam-1 panel fields wrong: %q %q %q", cams[0].VideoModeLabel(), cams[0].HDRTypeLabel(), cams[0].PackageCameraLabel())
	}
	if cams[1].ID != "12345" || cams[1].Name != "Drifted" || cams[1].IsOnline() {
		t.Errorf("drifted item not recovered from raw: %+v", cams[1])
	}
	// Absent optional fields degrade to "" (the UI renders "-").
	if cams[2].ModelLabel() != "" || cams[2].IPLabel() != "" || cams[2].FirmwareLabel() != "" || cams[2].MACLabel() != "" {
		t.Errorf("absent fields should be empty: %+v", cams[2])
	}
	if cams[2].Raw == nil {
		t.Errorf("raw map missing")
	}
}

// A data-wrapped list decodes too (firmware re-wrap tolerance), and a
// present-but-null data key reads as an empty list.
func TestListCameras_DataWrapper(t *testing.T) {
	body := `{"data":[{"id":"cam-1","name":"Front","state":"CONNECTED"}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, APIKey: "k"})
	cams, err := c.ListCameras(context.Background())
	if err != nil || len(cams) != 1 {
		t.Fatalf("cams=%v err=%v", cams, err)
	}

	body = `{"data":null}`
	cams, err = c.ListCameras(context.Background())
	if err != nil || len(cams) != 0 {
		t.Fatalf("data:null -> cams=%v err=%v, want empty list", cams, err)
	}
}

// A redirect is never followed: Go copies CUSTOM headers (X-API-KEY)
// onto cross-host redirects, so following one would hand the key to
// the redirect target.
func TestClient_DoesNotFollowRedirects(t *testing.T) {
	var leaked int32
	leakTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&leaked, 1)
	}))
	defer leakTarget.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leakTarget.URL, http.StatusFound)
	}))
	defer ts.Close()

	c := New(Options{BaseURL: ts.URL, APIKey: "k"})
	_, err := c.ListCameras(context.Background())
	if err == nil || !strings.Contains(err.Error(), "http 302") {
		t.Errorf("want bare http-302 error, got %v", err)
	}
	if n := atomic.LoadInt32(&leaked); n != 0 {
		t.Errorf("redirect was followed %d times - the X-API-KEY would have leaked", n)
	}
}

// The sensors decode reads the documented nested blocks and survives
// a completely drifted stats shape.
func TestListSensors_FieldsAndTolerance(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id":"sen-1","name":"Keller","state":"CONNECTED","mac":"AA:BB:CC:00:11:33",
			 "mountType":"leak","isOpened":false,"isMotionDetected":true,
			 "stats":{"temperature":{"value":21.5,"status":"neutral"},"humidity":{"value":48,"status":"neutral"},"light":{"value":120,"status":"neutral"}},
			 "batteryStatus":{"percentage":87,"isLow":false},
			 "wirelessConnectionState":{"signalState":"good","signalStrength":-62,"bridge":"bridge-1"}},
			{"id":"sen-2","name":"Drift","state":"CONNECTED","stats":"totally-not-an-object","batteryStatus":{"percentage":12,"isLow":true}}
		]`))
	}))
	defer ts.Close()
	c := New(Options{BaseURL: ts.URL, APIKey: "k"})

	sens, err := c.ListSensors(context.Background())
	if err != nil {
		t.Fatalf("ListSensors: %v", err)
	}
	if len(sens) != 2 {
		t.Fatalf("got %d sensors, want 2", len(sens))
	}
	s := sens[0]
	for got, want := range map[string]string{
		s.TemperatureLabel(): "21.5 °C",
		s.HumidityLabel():    "48 %",
		s.LightLabel():       "120 lx",
		s.MotionLabel():      "Yes",
		s.SignalLabel():      "good (-62 dBm)",
		s.BatteryLabel():     "87 %",
		s.BridgeLabel():      "bridge-1",
		s.MountTypeLabel():   "leak",
		s.OpenedLabel():      "No",
	} {
		if got != want {
			t.Errorf("label = %q, want %q", got, want)
		}
	}
	// The drifted stats shape must not kill the sensor - it just has
	// no measurements, and the rest of the record stays readable.
	d := sens[1]
	if d.TemperatureLabel() != "" || d.HumidityLabel() != "" {
		t.Errorf("drifted stats should read absent: %q %q", d.TemperatureLabel(), d.HumidityLabel())
	}
	if d.BatteryLabel() != "12 % (low)" {
		t.Errorf("battery = %q, want %q", d.BatteryLabel(), "12 % (low)")
	}
}

// Leak/tamper labels: fresh timestamps read "Yes" (also when slightly
// in the future - NVR clock skew must not suppress a live event),
// stale ones a qualified "No (last <age>)", absent fields "" ("-").
func TestSensor_EventRecency(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	fresh := flexJSON(t, now.Add(-recentEventWindow/2).UnixMilli())
	skewed := flexJSON(t, now.Add(recentEventWindow/2).UnixMilli()) // NVR clock ahead
	stale := flexJSON(t, now.Add(-2*recentEventWindow).UnixMilli())

	s := Sensor{LeakDetectedAt: fresh, TamperingDetectedAt: stale}
	if got := s.LeakLabel(now); got != "Yes" {
		t.Errorf("fresh leak = %q, want Yes", got)
	}
	if got := s.TamperLabel(now); got != "No (last 20 min ago)" {
		t.Errorf("stale tamper = %q, want %q", got, "No (last 20 min ago)")
	}
	skewSensor := Sensor{LeakDetectedAt: skewed}
	if got := skewSensor.LeakLabel(now); got != "Yes" {
		t.Errorf("future-skewed leak = %q, want Yes", got)
	}
	var empty Sensor
	if got := empty.LeakLabel(now); got != "" {
		t.Errorf("absent leak = %q, want empty", got)
	}
}

// Drifted non-scalar measurement values read as absent ("-"), never as
// raw JSON with a unit glued on.
func TestSensor_DriftedLeafValues(t *testing.T) {
	s := Sensor{
		Stats:   SensorStats{Temperature: SensorStat{Value: flexJSON(t, map[string]any{"v": 21.5})}},
		Battery: BatteryStatus{Percentage: flexJSON(t, []int{87})},
	}
	if got := s.TemperatureLabel(); got != "" {
		t.Errorf("object temperature = %q, want empty", got)
	}
	if got := s.BatteryLabel(); got != "" {
		t.Errorf("array battery = %q, want empty", got)
	}
}

// flexJSON builds a flexVal from a JSON-marshalable value.
func flexJSON(t *testing.T, v any) flexVal {
	t.Helper()
	var f flexVal
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := f.UnmarshalJSON(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return f
}
