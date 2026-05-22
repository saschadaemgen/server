package httpserver

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// designLibraryFS is the design-library bundle. tokens.css,
// components.css and interactions.js are exposed verbatim under
// /static/; the HTML snippets are parsed as Go templates by
// templates.go (same embed FS, different consumer).
//
//go:embed design-library/*
var designLibraryFS embed.FS

// staticHandler serves the css + js assets from the embedded
// design-library at /static/<name>. The HTML snippets in the
// same directory are NOT served as static files; they are
// parsed into the html/template registry instead.
func staticHandler() http.Handler {
	sub, err := fs.Sub(designLibraryFS, "design-library")
	if err != nil {
		panic("staticHandler: " + err.Error())
	}
	file := http.FileServer(http.FS(sub))
	return http.StripPrefix("/static/", assetGate(file))
}

// assetGate adds Cache-Control headers for the CSS/JS bundle and
// rejects requests for the HTML snippets so a misguided client
// cannot scrape the template source through /static/.
func assetGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=3600")
		case strings.HasSuffix(r.URL.Path, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=3600")
		case strings.HasSuffix(r.URL.Path, ".html"),
			strings.HasSuffix(r.URL.Path, ".md"):
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
