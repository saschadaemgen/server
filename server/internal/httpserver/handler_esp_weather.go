// Saison 14-XX: GET /esp/weather.
//
// Bearer-Auth-gating, identische Daten wie /webviewer/weather
// (open-meteo-Snapshot via internal/weather, 15-min Cache,
// 24h stale-serving auf API-Outages). Multi-Standort kommt in
// einer spaeteren Saison; bis dahin liefern beide Endpoints die
// gleiche station_lat/lon-Schnappschuss-Antwort.
package httpserver

import (
	"net/http"
)

// handleESPWeather ist ein duenner Bearer-gated-Re-Use des
// gemeinsamen Weather-Handlers. Identische Response-Form wie
// /webviewer/weather; nur die Auth-Mechanik unterscheidet sich.
func (s *Server) handleESPWeather(w http.ResponseWriter, r *http.Request) {
	if mac := ESPMACFromContext(r.Context()); mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	s.handleWeather(w, r)
}
