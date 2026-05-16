// Saison 14-01b: weather endpoints shared by mieter and admin.
//
//   GET /einloggen/weather   tenant pull (session cookie auth);
//                            consumed by idle.js every 15 minutes
//                            to refresh the screensaver block.
//   GET /a/weather           admin pull (admin session); useful
//                            from /a/settings to verify the
//                            saved station_lat/lon are correct.
//
// Both routes share a single handler. Auth differs (each route
// is wrapped in its own middleware in server.go); the response
// shape is identical. ErrUnavailable from the weather package
// maps to 503 with an empty JSON object so the browser knows to
// hide the weather block without throwing.
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/weather"
)

// stationCoords reads the operator's site coordinates from
// platform_config. The migration seeds them to Recklinghausen
// (51.6144, 7.1959) so the screensaver works on a fresh install
// without admin interaction. Parse errors fall back to those
// defaults so a typo in /a/settings cannot brick the page.
const (
	defaultStationLat = 51.6144
	defaultStationLon = 7.1959
)

func (s *Server) stationCoords(r *http.Request) (lat, lon float64) {
	lat, lon = defaultStationLat, defaultStationLon
	if s.platformCfg == nil {
		return
	}
	if v, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyStationLat); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			lat = f
		}
	}
	if v, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyStationLon); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			lon = f
		}
	}
	return
}

func (s *Server) handleWeather(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if s.weather == nil {
		// Backend not configured (shouldn't happen in normal boot,
		// but stay defensive so the page degrades cleanly).
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{})
		return
	}
	lat, lon := s.stationCoords(r)
	snap, err := s.weather.Get(r.Context(), lat, lon)
	if err != nil {
		if errors.Is(err, weather.ErrUnavailable) {
			s.log.Debug("weather unavailable", "lat", lat, "lon", lon, "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		s.log.Warn("weather fetch failed", "lat", lat, "lon", lon, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{})
		return
	}
	_ = json.NewEncoder(w).Encode(snap)
}
