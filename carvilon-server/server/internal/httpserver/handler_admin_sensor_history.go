package httpserver

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
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
