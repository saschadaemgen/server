package httpserver

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
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

// assetVersion is the cache-busting token the templates append
// to every /static/ URL (?v=<token>). It is computed once at
// process start from the embedded design-library, so a binary
// that ships different CSS/JS also ships different asset URLs
// and a browser cache can never pin stale assets across a
// deploy.
var assetVersion = mustAssetVersion(designLibraryFS)

// assetVersionFor derives the version token for an asset tree: a
// SHA-256 over the path and content of every regular file,
// truncated to 16 hex characters. fs.WalkDir visits entries in
// lexical order, so the token is deterministic for identical
// trees.
func assetVersionFor(fsys fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		body, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		// NUL separators keep the (path, content) boundaries
		// unambiguous, so a rename cannot alias a content edit.
		h.Write([]byte(path))
		h.Write([]byte{0})
		h.Write(body)
		h.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// mustAssetVersion panics on error: the input is the embedded FS
// baked into the binary, so a read failure is a build defect,
// not a runtime condition.
func mustAssetVersion(fsys fs.FS) string {
	v, err := assetVersionFor(fsys)
	if err != nil {
		panic("assetVersion: " + err.Error())
	}
	return v
}

// assetURL appends the version token to a /static/ path. Exposed
// to the admin and viewer shells as the "asset" template
// function: {{asset "/static/tokens.css"}}.
func assetURL(path string) string {
	return path + "?v=" + assetVersion
}

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
//
// Versioned requests (?v=<token>, the only form the templates
// emit) are immutable: the URL changes whenever the embedded
// assets change, so browsers may cache them for a year.
// Unversioned requests carry no busting signal and must
// revalidate on every use.
func assetGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Header().Set("Cache-Control", assetCacheControl(r))
		case strings.HasSuffix(r.URL.Path, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", assetCacheControl(r))
		case strings.HasSuffix(r.URL.Path, ".html"),
			strings.HasSuffix(r.URL.Path, ".md"):
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// assetCacheControl picks the cache policy for one asset request.
func assetCacheControl(r *http.Request) string {
	if r.URL.Query().Get("v") != "" {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}
