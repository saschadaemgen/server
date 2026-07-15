package httpserver

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carvilon.local/server/internal/sensorhistory"
	"carvilon.local/server/web/designer"
)

// handleSensorHistory serves the stored-path query API (Sensor History H1)
// that H2 charts consume: a device's metric over [from, to] as averaged
// interval buckets, downsampled to at most `points` when the range is wide.
// GET /a/devices/sensors/history?device=<id>&metric=<m>&from=<ms>&to=<ms>&points=<n>
func (s *Server) handleSensorHistory(w http.ResponseWriter, r *http.Request) {
	if s.sensorHistory == nil {
		http.Error(w, "sensor history not available", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	device := strings.TrimSpace(q.Get("device"))
	metric := strings.TrimSpace(q.Get("metric"))
	if device == "" || metric == "" {
		http.Error(w, "device and metric are required", http.StatusBadRequest)
		return
	}
	now := time.Now().UnixMilli()
	from := parseMillis(q.Get("from"), 0)
	to := parseMillis(q.Get("to"), now)
	points := int(parseMillis(q.Get("points"), 1000))
	if points > 5000 { // guard against an accidental huge raw pull
		points = 5000
	}
	if points < 1 {
		// The Store reads maxPoints <= 0 as "downsampling off", which over a
		// year of second-resolution history is an unbounded row pull. That is
		// a legitimate library call but must not be reachable from a query
		// string, so the HTTP edge always asks for a bounded series.
		points = 1
	}
	samples, err := s.sensorHistory.Query(r.Context(), device, metric, from, to, points)
	if err != nil {
		s.log.Error("sensor history query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device":  device,
		"metric":  metric,
		"from":    from,
		"to":      to,
		"samples": samples,
	})
}

// sensorMetricRow is one chartable metric of a device, as the charts (H2)
// need it: the capability catalog's presentation (label/unit/kind) joined to
// what the store actually holds. Recorded reports whether any sample exists,
// so a chart shows an honest "not recorded yet" instead of an empty axis;
// First/Last are the retained extent, which is what the "all" range spans.
type sensorMetricRow struct {
	Metric   string `json:"metric"`
	Label    string `json:"label"`
	Unit     string `json:"unit,omitempty"`
	Kind     string `json:"kind"`
	Recorded bool   `json:"recorded"`
	First    int64  `json:"first,omitempty"`
	Last     int64  `json:"last,omitempty"`
	N        int64  `json:"n,omitempty"`
}

// handleSensorMetrics serves the metric list for one device: which metrics
// are chartable and which of them have stored history. Both chart homes (the
// devices cockpit and the Logic Editor's sensor block) read this, so neither
// hardcodes a vendor's metric names - a family that declares readouts and
// feeds the recorder gets charts through this path for free.
//
// The list is a UNION of two views, because neither alone is honest: the
// capability catalog is live-only and present-filtered (an offline sensor
// vanishes from it while its stored history remains queryable), and the
// store knows tokens but carries no label or unit (sensor_samples has no
// unit column). Recorded metrics lead; declared-but-never-recorded metrics
// follow so the UI can name what is missing.
// GET /a/devices/sensors/metrics?device=<id>
func (s *Server) handleSensorMetrics(w http.ResponseWriter, r *http.Request) {
	if s.sensorHistory == nil {
		http.Error(w, "sensor history not available", http.StatusServiceUnavailable)
		return
	}
	device := strings.TrimSpace(r.URL.Query().Get("device"))
	if device == "" {
		http.Error(w, "device is required", http.StatusBadRequest)
		return
	}
	spans, err := s.sensorHistory.Metrics(r.Context(), device)
	if err != nil {
		s.log.Error("sensor metrics query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	stored := make(map[string]sensorhistory.MetricSpan, len(spans))
	for _, sp := range spans {
		stored[sp.Metric] = sp
	}

	// Capability side: label/unit/kind, and the declared metrics that have no
	// samples yet. Text readouts are dropped - a chart plots numbers, and the
	// recorder skips them for the same reason.
	caps := map[string]designer.ReadoutPort{}
	var order []string
	var name, class string
	for _, d := range s.readoutDevicesForCatalog(r.Context()) {
		if d.ID != device {
			continue
		}
		name, class = d.Name, d.Class
		for _, ro := range d.Readouts {
			if ro.Kind == "text" {
				continue
			}
			if _, dup := caps[ro.Key]; !dup {
				caps[ro.Key] = ro
				order = append(order, ro.Key)
			}
		}
	}

	out := make([]sensorMetricRow, 0, len(order)+len(spans))
	seen := map[string]bool{}
	add := func(token string) {
		if seen[token] {
			return
		}
		seen[token] = true
		row := sensorMetricRow{Metric: token, Label: token, Kind: "float"}
		if c, ok := caps[token]; ok {
			row.Label, row.Unit = c.Label, c.Unit
			if c.Kind != "" {
				row.Kind = c.Kind
			}
		}
		if sp, ok := stored[token]; ok {
			row.Recorded, row.First, row.Last, row.N = true, sp.First, sp.Last, sp.N
		}
		out = append(out, row)
	}
	// Recorded first, in the catalog's declared order where known, so the
	// charts stack in the order the faceplate lists them.
	for _, k := range order {
		if _, ok := stored[k]; ok {
			add(k)
		}
	}
	for _, sp := range spans {
		add(sp.Metric)
	}
	for _, k := range order {
		add(k)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device":  device,
		"name":    name,
		"class":   class,
		"metrics": out,
	})
}

// handleSensorRecordingSave persists a sensor's per-device recording knobs
// (interval + retention). A value of 0 clears that knob back to the global
// default; the config store clamps the interval to the allowed range.
// POST /a/devices/sensors/recording (device, interval_sec, retention_sec)
func (s *Server) handleSensorRecordingSave(w http.ResponseWriter, r *http.Request) {
	if s.sensorHistCfg == nil {
		http.Redirect(w, r, "/a/devices", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/a/devices?flash=rec-err", http.StatusSeeOther)
		return
	}
	device := strings.TrimSpace(r.PostFormValue("device"))
	if device == "" {
		http.Redirect(w, r, "/a/devices?flash=rec-err", http.StatusSeeOther)
		return
	}
	interval := parseMillis(r.PostFormValue("interval_sec"), 0)
	retention := parseMillis(r.PostFormValue("retention_sec"), 0)
	if err := s.sensorHistCfg.Set(r.Context(), device, interval, retention); err != nil {
		s.log.Error("sensor recording config save failed", "err", err)
		http.Redirect(w, r, "/a/devices?flash=rec-err", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/a/devices?flash=rec-saved", http.StatusSeeOther)
}

// parseMillis parses a base-10 int64 query/form value, returning def on any
// parse error or empty input.
func parseMillis(s string, def int64) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}
