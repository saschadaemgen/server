package httpserver

import "net/http"

// placeholderData ist die Payload fuer die "kommt bald"-Seiten
// (esp-viewers, users, esp-pager). Saison 13-02-FIX4-a haengt
// diese Routen schon im Menue, damit der naechste Sub-Brief
// (FIX4-b/c/d) sie nur noch mit Funktionalitaet fuellen muss.
type placeholderData struct {
	User       adminUser
	Title      string
	Lead       string
	CardHead   string
	CardBody   string
	NextBrief  string
}

func (s *Server) renderPlaceholder(w http.ResponseWriter, r *http.Request, name string, data placeholderData) {
	username := AdminUserFromContext(r.Context())
	data.User = adminUser{Name: username, Initials: initialsOf(username)}
	s.renderAdminPage(w, name, data)
}

func (s *Server) handleAdminEspViewers(w http.ResponseWriter, r *http.Request) {
	s.renderPlaceholder(w, r, "esp-viewers", placeholderData{
		Title:    "ESP-Viewer",
		Lead:     "Adoptierte ESP32-Hardware-Viewer als Klingel-Empfaenger.",
		CardHead: "Geplant fuer FIX4-c",
		CardBody: "Anlegen, Token-Pairing und Status-Monitor fuer ESP32-Viewer-Geraete. Hardware-Stack laeuft als kompatible Alternative zum Web-Viewer im Browser.",
		NextBrief: "S13-02-FIX4-c",
	})
}

func (s *Server) handleAdminEspPager(w http.ResponseWriter, r *http.Request) {
	s.renderPlaceholder(w, r, "esp-pager", placeholderData{
		Title:    "ESP-Pager",
		Lead:     "Portable ESP32-basierte Pager mit Akku fuer mobile Benachrichtigungen.",
		CardHead: "Geplant fuer kommende Saison",
		CardBody: "Kleine portable Geraete mit Akku, die der Hausmeister oder Vermieter mit sich tragen kann. Wenn an einer Klingel im verwalteten Bestand etwas passiert, vibriert der Pager und zeigt einen Hinweis auf einem kleinen E-Paper-Display.",
		NextBrief: "Saison-Backlog",
	})
}
