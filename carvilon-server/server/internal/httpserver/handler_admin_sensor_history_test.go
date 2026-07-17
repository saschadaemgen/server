package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/sensorhistory"
)

func newSensorHistServer(t *testing.T) (*Server, *sensorhistory.Store) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	st := sensorhistory.New(d.DB)
	// No monitors: readoutDevicesForCatalog is nil-safe, so the metrics
	// endpoint falls back to the store-only view - which is exactly the
	// offline-device case worth pinning.
	return &Server{sensorHistory: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}, st
}

// A device's history is only reachable if the UI can discover which metrics it
// has; the store half of that union must survive with no live monitor.
func TestHandleSensorMetrics_ListsRecordedMetrics(t *testing.T) {
	s, st := newSensorHistServer(t)
	ctx := context.Background()
	if err := st.Insert(ctx,
		sensorhistory.Sample{DeviceID: "sen-1", Metric: "temperature", TS: 1000, Value: 21, N: 2},
		sensorhistory.Sample{DeviceID: "sen-1", Metric: "temperature", TS: 5000, Value: 23, N: 3},
		sensorhistory.Sample{DeviceID: "sen-1", Metric: "humidity", TS: 2000, Value: 55, N: 1},
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rr := httptest.NewRecorder()
	s.handleSensorMetrics(rr, httptest.NewRequest(http.MethodGet, "/a/devices/sensors/metrics?device=sen-1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var got struct {
		Device  string            `json:"device"`
		Metrics []sensorMetricRow `json:"metrics"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if got.Device != "sen-1" || len(got.Metrics) != 2 {
		t.Fatalf("metrics = %+v, want 2 for sen-1", got.Metrics)
	}
	byName := map[string]sensorMetricRow{}
	for _, m := range got.Metrics {
		byName[m.Metric] = m
	}
	temp, ok := byName["temperature"]
	if !ok {
		t.Fatalf("temperature missing: %+v", got.Metrics)
	}
	if !temp.Recorded || temp.First != 1000 || temp.Last != 5000 || temp.N != 2 {
		t.Errorf("temperature row = %+v, want recorded span 1000..5000 n=2", temp)
	}
	// With no catalog to join, the token stands in for the label rather than
	// the row vanishing.
	if temp.Label != "temperature" || temp.Kind != "float" {
		t.Errorf("temperature fallback presentation = %+v", temp)
	}
	if _, ok := byName["humidity"]; !ok {
		t.Errorf("humidity missing: %+v", got.Metrics)
	}

	// An unknown device is an empty list, not an error - a sensor adopted a
	// minute ago simply has nothing yet.
	rr = httptest.NewRecorder()
	s.handleSensorMetrics(rr, httptest.NewRequest(http.MethodGet, "/a/devices/sensors/metrics?device=nope", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("unknown device status = %d, want 200", rr.Code)
	}
	var empty struct {
		Metrics []sensorMetricRow `json:"metrics"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &empty)
	if len(empty.Metrics) != 0 {
		t.Errorf("unknown device metrics = %+v, want none", empty.Metrics)
	}
}

func TestHandleSensorMetrics_RequiresDevice(t *testing.T) {
	s, _ := newSensorHistServer(t)
	rr := httptest.NewRecorder()
	s.handleSensorMetrics(rr, httptest.NewRequest(http.MethodGet, "/a/devices/sensors/metrics", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// The Store reads maxPoints <= 0 as "downsampling off" - a legitimate library
// call, but over a long retention it is an unbounded row pull. It must not be
// reachable from a query string.
func TestHandleSensorHistory_PointsClampedToBoundedSeries(t *testing.T) {
	s, st := newSensorHistServer(t)
	ctx := context.Background()
	samples := make([]sensorhistory.Sample, 500)
	for i := range samples {
		samples[i] = sensorhistory.Sample{DeviceID: "s", Metric: "m", TS: int64(i) * 1000, Value: float64(i), N: 1}
	}
	if err := st.Insert(ctx, samples...); err != nil {
		t.Fatalf("insert: %v", err)
	}

	for _, points := range []string{"0", "-5"} {
		rr := httptest.NewRecorder()
		s.handleSensorHistory(rr, httptest.NewRequest(http.MethodGet,
			"/a/devices/sensors/history?device=s&metric=m&from=0&to=999000&points="+points, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("points=%s status = %d, want 200", points, rr.Code)
		}
		var got struct {
			Samples []sensorhistory.Sample `json:"samples"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Samples) >= 500 {
			t.Errorf("points=%s returned %d samples: the raw pull is reachable from the query string", points, len(got.Samples))
		}
	}

	// The documented cap still holds at the top end.
	rr := httptest.NewRecorder()
	s.handleSensorHistory(rr, httptest.NewRequest(http.MethodGet,
		"/a/devices/sensors/history?device=s&metric=m&from=0&to=999000&points=99999", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestHandleSensorHistory_Unavailable(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rr := httptest.NewRecorder()
	s.handleSensorMetrics(rr, httptest.NewRequest(http.MethodGet, "/a/devices/sensors/metrics?device=x", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when history is not wired", rr.Code)
	}
}

// Regression (Midea charts vanished after the Shelly work): the metrics
// endpoint must still surface a Midea device's recorded metrics. Midea keys on
// its own device id (not a shelly-<mac> id), and even with no live monitor to
// supply labels, a recorded metric must come back recorded:true so the chart
// renders it.
func TestHandleSensorMetrics_MideaRecordedMetricsStillSurface(t *testing.T) {
	s, st := newSensorHistServer(t)
	ctx := context.Background()
	const mid = "1122334455667788" // a Midea device id (placeholder)
	if err := st.Insert(ctx,
		sensorhistory.Sample{DeviceID: mid, Metric: "device_temp", TS: 1000, Value: 23.4, N: 3},
		sensorhistory.Sample{DeviceID: mid, Metric: "device_temp", TS: 61000, Value: 23.6, N: 4},
		sensorhistory.Sample{DeviceID: mid, Metric: "outdoor_temp", TS: 1000, Value: 31.2, N: 3},
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rr := httptest.NewRecorder()
	s.handleSensorMetrics(rr, httptest.NewRequest(http.MethodGet, "/a/devices/sensors/metrics?device="+mid, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var got struct {
		Metrics []sensorMetricRow `json:"metrics"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec := map[string]bool{}
	for _, m := range got.Metrics {
		if m.Recorded {
			rec[m.Metric] = true
		}
	}
	if !rec["device_temp"] || !rec["outdoor_temp"] {
		t.Fatalf("Midea recorded metrics not surfaced as recorded: %+v", got.Metrics)
	}
}
