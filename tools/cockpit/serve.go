// CARVILON Cockpit — dependency-free local static server.
//
// Serves the cockpit shell (public/) and the tracked cockpit content
// (content/), and reads the *gitignored* living-docs at their existing local
// paths at runtime (never copying them into the tracked tree). It maps a small
// set of named, read-only "doc roots" to wherever those docs physically live
// after the monorepo merge, and serves ONLY *.md from them (so the .har / .pdf /
// .env that sit next to some docs are never exposed).
//
// Start (one line, from anywhere):
//
//	go run ./tools/cockpit/serve.go
//
// Then open http://127.0.0.1:7878 . Binds to localhost only on purpose: the
// living docs carry real infrastructure data and must not reach the LAN.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// docRoots maps a stable URL name -> ordered candidate directories. The first
// candidate that exists on disk wins. Candidates cover both the canonical
// monorepo layout and the retired pre-merge source folders, because the
// gitignored carvilon living-docs were never grafted into server/ (git subtree
// only carries tracked files) and currently live at the retired source.
func resolveDocRoots(repoRoot, projectRoot string) map[string]string {
	candidates := map[string][]string{
		"carvilon-docs": {
			filepath.Join(repoRoot, "carvilon-server", "docs"),
			filepath.Join(projectRoot, "carvilon-server", "docs"),
		},
		"carvilon-seasons": {
			filepath.Join(repoRoot, "carvilon-server", "seasons"),
			filepath.Join(projectRoot, "carvilon-server", "seasons"),
		},
		"stream-docs":    {filepath.Join(repoRoot, "streaming-server", "docs")},
		"stream-seasons": {filepath.Join(repoRoot, "streaming-server", "seasons")},
		"project-docs":   {filepath.Join(projectRoot, "docs")},
	}
	resolved := map[string]string{}
	for name, cands := range candidates {
		for _, c := range cands {
			if info, err := os.Stat(c); err == nil && info.IsDir() {
				resolved[name] = c
				break
			}
		}
	}
	return resolved
}

func main() {
	addr := flag.String("addr", "127.0.0.1:7878", "listen address (localhost only by design)")
	dirFlag := flag.String("dir", "", "cockpit dir override (default: location of serve.go)")
	flag.Parse()

	cockpitDir := *dirFlag
	if cockpitDir == "" {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			log.Fatal("cannot locate serve.go; pass -dir")
		}
		cockpitDir = filepath.Dir(thisFile)
	}
	cockpitDir, _ = filepath.Abs(cockpitDir)
	repoRoot := filepath.Dir(filepath.Dir(cockpitDir)) // .../server
	projectRoot := filepath.Dir(repoRoot)              // .../Carvilon

	publicDir := filepath.Join(cockpitDir, "public")
	contentDir := filepath.Join(cockpitDir, "content")
	if _, err := os.Stat(filepath.Join(publicDir, "index.html")); err != nil {
		log.Fatalf("public/index.html not found under %s (wrong -dir?)", cockpitDir)
	}

	docRoots := resolveDocRoots(repoRoot, projectRoot)

	mux := http.NewServeMux()

	// Shell + tracked cockpit content (system-map.js, manifest.js, notes/*.md).
	mux.Handle("/", http.FileServer(http.Dir(publicDir)))
	mux.Handle("/content/", http.StripPrefix("/content/", http.FileServer(http.Dir(contentDir))))

	// Living-doc roots: read-only, *.md only, no directory listings.
	for name, dir := range docRoots {
		prefix := "/docs/" + name + "/"
		mux.Handle(prefix, http.StripPrefix(prefix, http.FileServer(mdOnlyDir(dir))))
	}

	// JSON: which roots resolved (diagnostics + UI hints).
	mux.HandleFunc("/api/roots", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]bool{}
		for _, n := range []string{"carvilon-docs", "carvilon-seasons", "stream-docs", "stream-seasons", "project-docs"} {
			_, out[n] = docRoots[n]
		}
		writeJSON(w, out)
	})

	// JSON: list the *.md files in a named root (drives the Archiv view).
	mux.HandleFunc("/api/list", func(w http.ResponseWriter, r *http.Request) {
		dir, ok := docRoots[r.URL.Query().Get("root")]
		if !ok {
			http.Error(w, "unknown or unresolved root", http.StatusNotFound)
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			http.Error(w, "cannot read root", http.StatusInternalServerError)
			return
		}
		type fileInfo struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		}
		var files []fileInfo
		for _, e := range entries {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
				continue
			}
			info, _ := e.Info()
			var size int64
			if info != nil {
				size = info.Size()
			}
			files = append(files, fileInfo{Name: e.Name(), Size: size})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		writeJSON(w, files)
	})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}

	fmt.Println("┌─────────────────────────────────────────────────────────────")
	fmt.Println("│  CARVILON Cockpit")
	fmt.Printf("│  → http://%s\n", *addr)
	fmt.Println("│")
	fmt.Printf("│  cockpit : %s\n", cockpitDir)
	fmt.Println("│  doc roots:")
	for _, n := range []string{"carvilon-docs", "carvilon-seasons", "stream-docs", "stream-seasons", "project-docs"} {
		if d, ok := docRoots[n]; ok {
			fmt.Printf("│    ✓ %-16s %s\n", n, d)
		} else {
			fmt.Printf("│    ✗ %-16s (not found — tab will show a hint)\n", n)
		}
	}
	fmt.Println("└─────────────────────────────────────────────────────────────")

	srv := &http.Server{Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.Serve(ln))
}

// mdOnlyDir is an http.FileSystem that exposes only *.md regular files and
// refuses directories (no listings, no traversal beyond the root).
type mdOnlyDir string

func (d mdOnlyDir) Open(name string) (http.File, error) {
	if strings.Contains(name, "..") {
		return nil, os.ErrNotExist
	}
	if name != "/" && !strings.EqualFold(filepath.Ext(name), ".md") {
		return nil, os.ErrNotExist
	}
	f, err := http.Dir(d).Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		log.Printf("%s %s", r.Method, r.URL.Path)
	})
}
