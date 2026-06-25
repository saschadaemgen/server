package httpserver

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"carvilon.local/server/web/designer"
)

// TestDesignerStaticHandler_ServesEditorIndex verifies the embedded
// editor bundle is served at the /a/designer/ root (index.html is the
// directory index).
func TestDesignerStaticHandler_ServesEditorIndex(t *testing.T) {
	h := designerStaticHandler()

	req := httptest.NewRequest(http.MethodGet, "/a/designer/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /a/designer/ = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "CARVILON") || !strings.Contains(body, "Logik-Editor") {
		t.Errorf("served body does not look like the editor index.html")
	}
}

// TestDesignerBundle_LocalFirst is the load-bearing guard: the editor
// must make no external request when it loads. It walks every embedded
// file (HTML shell, css/, the js/ ES modules, vendored CSS) so any
// reintroduced CDN / Google-Fonts reference anywhere in the bundle fails
// here.
func TestDesignerBundle_LocalFirst(t *testing.T) {
	banned := []string{"unpkg.com", "fonts.googleapis.com", "fonts.gstatic.com", "cdn.jsdelivr.net", "cdnjs.cloudflare.com"}
	err := fs.WalkDir(designer.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(path, ".woff2") {
			return nil
		}
		data, err := fs.ReadFile(designer.FS, path)
		if err != nil {
			return err
		}
		body := string(data)
		for _, b := range banned {
			if strings.Contains(body, b) {
				t.Errorf("%s references external host %q — local-first violated", path, b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking designer FS: %v", err)
	}
}

// TestDesignerBundle_ModuleEntry verifies the thin index.html shell loads
// the CSS + ES-module entry, and that both are served with a usable
// content type (set explicitly so module scripts pass strict MIME checks
// regardless of the host OS mime table).
func TestDesignerBundle_ModuleEntry(t *testing.T) {
	h := designerStaticHandler()

	idx := httptest.NewRequest(http.MethodGet, "/a/designer/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, idx)
	body := rec.Body.String()
	for _, want := range []string{`href="./css/editor.css"`, `type="module" src="./js/main.js"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html shell missing %q", want)
		}
	}

	cases := []struct{ path, wantSubstr string }{
		{"/a/designer/js/main.js", "javascript"},
		{"/a/designer/js/store.js", "javascript"},
		{"/a/designer/css/editor.css", "text/css"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		if r.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", c.path, r.Code)
			continue
		}
		if ct := r.Header().Get("Content-Type"); !strings.Contains(ct, c.wantSubstr) {
			t.Errorf("GET %s content-type = %q, want substring %q", c.path, ct, c.wantSubstr)
		}
		_, _ = io.Copy(io.Discard, r.Body)
	}
}

// TestDesignerStaticHandler_VendorContentTypes verifies the vendored
// JS and woff2 assets are served with usable content types (woff2 is
// set explicitly because Go's mime table does not always carry it).
func TestDesignerStaticHandler_VendorContentTypes(t *testing.T) {
	h := designerStaticHandler()

	cases := []struct {
		path       string
		wantSubstr string
	}{
		{"/a/designer/vendor/lucide.min.js", "javascript"},
		{"/a/designer/vendor/fonts.css", "text/css"},
		{"/a/designer/vendor/fonts/UcC73FwrK3iLTeHuS_nVMrMxCp50SjIa1ZL7.woff2", "font/woff2"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", c.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, c.wantSubstr) {
			t.Errorf("GET %s content-type = %q, want substring %q", c.path, ct, c.wantSubstr)
		}
		// drain to mirror a real client read
		_, _ = io.Copy(io.Discard, rec.Body)
	}
}
