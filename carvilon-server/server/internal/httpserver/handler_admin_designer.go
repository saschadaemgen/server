package httpserver

import (
	"net/http"
	"strings"

	"carvilon.local/server/web/designer"
)

// designerData is the payload for the /a/designer host page. The editor
// itself lives entirely inside the iframe (its own document, CSS and
// JS); the host page only needs the admin user for the shared topbar.
type designerData struct {
	User adminUser
}

// handleAdminDesigner renders the logic-editor host page: the shared
// Saison-20 admin layout (topbar) wrapping a full-bleed iframe that
// loads the embedded editor from /a/designer/. The iframe gives the
// editor a clean isolation boundary from the admin shell's tokens and
// scripts.
func (s *Server) handleAdminDesigner(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	s.renderAdminPage(w, "designer", designerData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
	})
}

// designerStaticHandler serves the embedded editor bundle under
// /a/designer/. index.html is the directory index; the vendored
// Lucide/font assets are served verbatim from the same FS. A request to
// /a/designer/ resolves to index.html.
//
// Content-Type for woff2 is set explicitly because Go's mime table does
// not always carry it (same reason the /static/ asset gate does so).
// The bundle is small and changes only on deploy, so a plain no-cache
// keeps it simple without a cache-busting token.
func designerStaticHandler() http.Handler {
	file := http.FileServer(http.FS(designer.FS))
	served := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".woff2") {
			w.Header().Set("Content-Type", "font/woff2")
		}
		w.Header().Set("Cache-Control", "no-cache")
		file.ServeHTTP(w, r)
	})
	return http.StripPrefix("/a/designer/", served)
}
