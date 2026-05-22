// ESP-side pendant to /webviewer/history*.
//
// Three endpoints, all bearer-gated:
//
//	GET    /esp/history.json          paged JSON list
//	DELETE /esp/history/{event_id}    soft-delete one
//	DELETE /esp/history               soft-delete all
//
// Pattern: the bearer lookup runs in the requireESPBearer
// middleware (server.go), so the MAC sits on the request
// context. The handlers below pull the MAC and delegate to the
// serveHistory* helpers in handler_mieter_history.go - identical
// validation, identical response shape, identical soft-delete
// semantics as on the Mieter web side. The only difference
// between mieter and ESP is the auth mechanism.
//
// /esp/settings additionally accepts history_capture (boolean);
// see handler_esp_settings.go for the ESP-only logic.
package httpserver

import (
	"net/http"
)

// handleESPHistoryList ist der Bearer-gated-Re-Use von
// serveHistoryList. Response 1:1 wie /webviewer/history.json:
// {events, has_more, next_offset, capture_enabled}.
//
// Route: GET /esp/history.json (requireESPBearer)
func (s *Server) handleESPHistoryList(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	s.serveHistoryList(w, r, mac)
}

// handleESPHistoryDeleteOne soft-hides einen einzelnen Eintrag
// fuer das ESP-Geraet. event_id steht im Pfad. Cross-Viewer-
// Schutz und 404-Mapping kommen aus dem doorhistory-Layer.
//
// Route: DELETE /esp/history/{event_id} (requireESPBearer)
func (s *Server) handleESPHistoryDeleteOne(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	s.serveHistoryHideOne(w, r, mac)
}

// handleESPHistoryDeleteAll soft-hides alle aktuell sichtbaren
// Eintraege fuer das ESP-Geraet. Idempotent.
//
// Route: DELETE /esp/history (requireESPBearer)
func (s *Server) handleESPHistoryDeleteAll(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	s.serveHistoryHideAll(w, r, mac)
}
