package httpserver

import (
	"encoding/json"
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

// handleDesignerCatalog serves the designer building-block catalog as
// JSON for the editor palette. Route: GET /a/designer/catalog.json
// (requireAdminSession) — the more specific pattern wins over the
// /a/designer/ static subtree. The catalog is the single source of
// truth for the 111 palette blocks; the four implemented ones derive
// their ports/params from the engine registry.
func (s *Server) handleDesignerCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{"blocks": designer.Catalog()})
}

// designerStaticHandler serves the embedded editor bundle under
// /a/designer/. index.html is the directory index; the ES modules under
// js/, the css/, and the vendored Lucide/font assets are served verbatim
// from the same FS. A request to /a/designer/ resolves to index.html.
//
// Content-Type is set explicitly per extension because Go's mime table
// is OS-dependent (the Windows registry can map .js to text/plain, which
// browsers reject for module scripts under strict MIME checking). woff2
// is set for the same reason the /static/ asset gate does. The bundle is
// small and changes only on deploy, so a plain no-cache keeps it simple
// without a cache-busting token.
func designerStaticHandler() http.Handler {
	file := http.FileServer(http.FS(designer.FS))
	served := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".woff2"):
			w.Header().Set("Content-Type", "font/woff2")
		case strings.HasSuffix(r.URL.Path, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case strings.HasSuffix(r.URL.Path, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "no-cache")
		file.ServeHTTP(w, r)
	})
	return http.StripPrefix("/a/designer/", served)
}
