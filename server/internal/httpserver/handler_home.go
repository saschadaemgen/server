package httpserver

import (
	"html/template"
	"net/http"
)

// homeTpl is the tenant landing-page stub. A later saison
// replaces it with the doorbell view.
var homeTpl = template.Must(template.New("home").Parse(`<!doctype html>
<html lang="de">
<head><meta charset="utf-8"><title>unifix Mieter</title></head>
<body>
<h1>Hallo</h1>
<p>Du bist eingeloggt als User-ID: {{.UAUserID}}</p>
<form method="POST" action="/m/logout">
  <button type="submit">Abmelden</button>
</form>
</body>
</html>
`))

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ua := UAUserIDFromContext(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = homeTpl.Execute(w, struct{ UAUserID string }{ua})
}
