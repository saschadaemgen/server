package httpserver

import "net/http"

// placeholderData is the payload for the "kommt bald" pages
// (esp-viewers, users, esp-pager). The routes are already wired
// in the nav so a future sub-briefing only has to fill them
// with functionality.
type placeholderData struct {
	User      adminUser
	Title     string
	Lead      string
	CardHead  string
	CardBody  string
	NextBrief string
}

func (s *Server) renderPlaceholder(w http.ResponseWriter, r *http.Request, name string, data placeholderData) {
	username := AdminUserFromContext(r.Context())
	data.User = adminUser{Name: username, Initials: initialsOf(username)}
	s.renderAdminPage(w, name, data)
}

func (s *Server) handleAdminEspPager(w http.ResponseWriter, r *http.Request) {
	s.renderPlaceholder(w, r, "esp-pager", placeholderData{
		Title:     "ESP-Pager",
		Lead:      "Portable ESP32-basierte Pager mit Akku fuer mobile Benachrichtigungen.",
		CardHead:  "Geplant fuer kommende Saison",
		CardBody:  "Kleine portable Geraete mit Akku, die der Hausmeister oder Vermieter mit sich tragen kann. Wenn an einer Klingel im verwalteten Bestand etwas passiert, vibriert der Pager und zeigt einen Hinweis auf einem kleinen E-Paper-Display.",
		NextBrief: "Saison-Backlog",
	})
}
