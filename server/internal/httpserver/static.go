package httpserver

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// staticFS holds the small set of files we ship verbatim to the
// browser: theme.css today, future fonts or images later. The
// embed directive bundles them into the binary so the server has
// no on-disk dependency.
//
//go:embed static/*
var staticFS embed.FS

// staticHandler returns an http.Handler that serves files from
// the embedded static/ directory under the /static/ URL prefix.
// All assets get a long Cache-Control because they are immutable
// per build; the binary version is the cache key.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// embed.FS guarantees the subtree exists at compile time;
		// a panic here means the build was broken intentionally.
		panic("staticHandler: " + err.Error())
	}
	file := http.FileServer(http.FS(sub))
	return http.StripPrefix("/static/", cacheableStatic(file))
}

// cacheableStatic adds Cache-Control to text/style responses so
// browsers do not refetch theme.css on every page load.
func cacheableStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		next.ServeHTTP(w, r)
	})
}
