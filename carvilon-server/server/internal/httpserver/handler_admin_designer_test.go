package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
// must make no external request when it loads. Any reintroduced CDN /
// Google-Fonts reference (in the HTML or the vendored CSS) fails here.
func TestDesignerBundle_LocalFirst(t *testing.T) {
	h := designerStaticHandler()

	banned := []string{"unpkg.com", "fonts.googleapis.com", "fonts.gstatic.com"}
	for _, p := range []string{"/a/designer/", "/a/designer/vendor/fonts.css"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", p, rec.Code)
		}
		body := rec.Body.String()
		for _, b := range banned {
			if strings.Contains(body, b) {
				t.Errorf("%s references external host %q — local-first violated", p, b)
			}
		}
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
