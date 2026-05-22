// GET /esp/weather.
//
// Bearer-auth gated reuse of the same data /webviewer/weather
// returns (open-meteo snapshot via internal/weather, 15 min
// cache, 24 h stale-serving on API outages). Multi-location
// support is a later-season topic; until then both endpoints
// return the same station_lat/lon snapshot.
package httpserver

import (
	"net/http"
)

// handleESPWeather is a thin bearer-gated reuse of the shared
// weather handler. Identical response shape to
// /webviewer/weather; only the auth mechanism differs.
func (s *Server) handleESPWeather(w http.ResponseWriter, r *http.Request) {
	if mac := ESPMACFromContext(r.Context()); mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	s.handleWeather(w, r)
}
