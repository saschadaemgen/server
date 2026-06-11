package httpserver

import (
	"bytes"
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAssetVersionForDeterministic(t *testing.T) {
	fsys := fstest.MapFS{
		"lib/a.css": {Data: []byte("body{}")},
		"lib/b.js":  {Data: []byte("console.log(1)")},
	}
	v1, err := assetVersionFor(fsys)
	if err != nil {
		t.Fatalf("assetVersionFor: %v", err)
	}
	v2, err := assetVersionFor(fsys)
	if err != nil {
		t.Fatalf("assetVersionFor (second run): %v", err)
	}
	if v1 != v2 {
		t.Errorf("same tree hashed twice: %q vs %q", v1, v2)
	}
}

func TestAssetVersionForContentSensitive(t *testing.T) {
	base := fstest.MapFS{
		"lib/a.css": {Data: []byte("body{}")},
		"lib/b.js":  {Data: []byte("console.log(1)")},
	}
	edited := fstest.MapFS{
		"lib/a.css": {Data: []byte("body{}")},
		"lib/b.js":  {Data: []byte("console.log(2)")},
	}
	v1, err := assetVersionFor(base)
	if err != nil {
		t.Fatalf("assetVersionFor(base): %v", err)
	}
	v2, err := assetVersionFor(edited)
	if err != nil {
		t.Fatalf("assetVersionFor(edited): %v", err)
	}
	if v1 == v2 {
		t.Errorf("content edit did not change version %q", v1)
	}
}

func TestAssetVersionForPathSensitive(t *testing.T) {
	// Same bytes under a different name must change the version;
	// the NUL framing also keeps boundary shifts (suffix of one
	// file moving into the path of the next) from aliasing.
	base := fstest.MapFS{
		"lib/a.css": {Data: []byte("body{}")},
	}
	renamed := fstest.MapFS{
		"lib/b.css": {Data: []byte("body{}")},
	}
	v1, err := assetVersionFor(base)
	if err != nil {
		t.Fatalf("assetVersionFor(base): %v", err)
	}
	v2, err := assetVersionFor(renamed)
	if err != nil {
		t.Fatalf("assetVersionFor(renamed): %v", err)
	}
	if v1 == v2 {
		t.Errorf("rename did not change version %q", v1)
	}
}

func TestAssetVersionFormat(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(assetVersion) {
		t.Errorf("assetVersion = %q, want 16 lowercase hex chars", assetVersion)
	}
}

func TestAssetURL(t *testing.T) {
	got := assetURL("/static/tokens.css")
	want := "/static/tokens.css?v=" + assetVersion
	if got != want {
		t.Errorf("assetURL = %q, want %q", got, want)
	}
}

// TestAssetGateCacheControl pins the cache policy: versioned
// asset URLs (the only form the templates emit) are immutable,
// unversioned ones must revalidate, and the template sources
// stay unreachable.
func TestAssetGateCacheControl(t *testing.T) {
	handler := staticHandler()
	cases := []struct {
		path       string
		wantStatus int
		wantCache  string
	}{
		{"/static/tokens.css?v=" + assetVersion, 200, "public, max-age=31536000, immutable"},
		{"/static/interactions.js?v=" + assetVersion, 200, "public, max-age=31536000, immutable"},
		{"/static/tokens.css", 200, "no-cache"},
		{"/static/interactions.js", 200, "no-cache"},
		{"/static/admin-login.html", 404, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != c.wantStatus {
			t.Errorf("GET %s: status = %d, want %d", c.path, rec.Code, c.wantStatus)
			continue
		}
		if c.wantCache == "" {
			continue
		}
		if got := rec.Header().Get("Cache-Control"); got != c.wantCache {
			t.Errorf("GET %s: Cache-Control = %q, want %q", c.path, got, c.wantCache)
		}
	}
}

// TestTemplatesEmitVersionedAssetURLs scans every embedded page
// template for raw /static/ references in src or href
// attributes. Each one must go through {{asset "..."}} or a
// deployed binary can serve fresh JSON to a browser still
// running cached stale JS.
func TestTemplatesEmitVersionedAssetURLs(t *testing.T) {
	raw := regexp.MustCompile(`(?:src|href)="/static/`)
	err := fs.WalkDir(templatesFS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		body, err := fs.ReadFile(templatesFS, path)
		if err != nil {
			return err
		}
		if raw.Match(body) {
			t.Errorf("%s references /static/ without the asset template function", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
}

// TestRenderedPageCarriesAssetVersion renders a real admin shell
// and asserts the versioned URL survives html/template's URL
// escaping intact (? and = must not be entity- or
// percent-encoded inside the attribute).
func TestRenderedPageCarriesAssetVersion(t *testing.T) {
	tpl, err := newAdminTemplates()
	if err != nil {
		t.Fatalf("newAdminTemplates: %v", err)
	}
	page := streamsPageData{User: adminUser{Name: "operator", Initials: "OP"}}
	env := navEnvelope{
		ActiveNav: navSlotFor("streams"),
		User:      extractUser(page),
		Page:      page,
	}
	var buf bytes.Buffer
	if err := tpl.renderPage(&buf, "streams", env); err != nil {
		t.Fatalf("render streams: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`href="/static/tokens.css?v=` + assetVersion + `"`,
		`href="/static/components.css?v=` + assetVersion + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered streams page missing %q", want)
		}
	}
}
