// Saison 14-04-Phase2-FIX06: ESP-Pendant zu /webviewer/history*.
//
// Drei Endpoints, alle Bearer-gated:
//
//	GET    /esp/history.json          paged JSON-Liste
//	DELETE /esp/history/{event_id}    Soft-Delete einzeln
//	DELETE /esp/history               Soft-Delete alle
//
// Pattern: Bearer-Lookup laeuft im requireESPBearer-Middleware
// (server.go), MAC sitzt im Request-Context. Hier holen wir den
// MAC raus und delegieren an die serveHistory*-Helfer aus
// handler_mieter_history.go - identische Validierung, identische
// Response-Form, identische Soft-Delete-Semantik wie auf der
// Mieter-Web-Seite. Der einzige Unterschied zwischen Mieter und
// ESP ist die Auth-Mechanik.
//
// /esp/settings akzeptiert ab FIX06 zusaetzlich history_capture
// (boolean); der dortige Handler liest die ESP-only-Logik (siehe
// handler_esp_settings.go).
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
