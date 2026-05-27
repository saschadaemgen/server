package httpserver

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"strings"
)

// htmlCommentRE matches HTML comment blocks. The design-library
// files start with extensive doc comments that contain example
// {{template "..." .}} action snippets. Go's html/template parses
// those actions even inside <!-- ... -->, so we strip the
// comments before handing the body to Parse.
var htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)

// libraryDemoOpenRE strips the `is-open` class from the modal,
// sheet, scrim and overlay elements where the library bakes it
// in as a standalone-demo convenience.
var libraryDemoOpenRE = regexp.MustCompile(`\bis-open\b`)

// libraryNavRE strips the library's own <nav class="admin-nav">
// ...</nav> block from the library snippets. The carvilon admin
// surface attaches its own nav (Web-Viewer, ESP-Viewer, Benutzer,
// ...) and cannot edit the legacy block (Mieter, Mock-Tools)
// directly inside the library files because the library is kept
// as an immutable design source.
var libraryNavRE = regexp.MustCompile(`(?s)<nav class="admin-nav".*?</nav>\s*`)

//go:embed templates/admin/*.html templates/viewer/*.html
var templatesFS embed.FS

// adminTemplates bundles every page template the http handlers
// can render. Each entry is a complete html/template tree that
// can run on its own via ExecuteTemplate(w, name, data).
type adminTemplates struct {
	pages  map[string]*template.Template
	viewer map[string]*template.Template
}

// macIDFromMAC turns a colon-separated MAC into a CSS-safe
// suffix for HTML id attributes.
func macIDFromMAC(mac string) string {
	return strings.ToLower(strings.ReplaceAll(mac, ":", ""))
}

// adminLibraryFor lists the design-library snippets each admin
// page shell needs. Only the login page uses a library card;
// every other admin page has its own slim template (the
// vocabulary + nav changes did not fit cleanly into the library
// snippets, and the library is kept immutable as a design
// source).
var adminLibraryFor = map[string][]string{
	"login":       {"admin-login.html"},
	"dashboard":   {},
	"settings":    {},
	"web-viewers": {},
	"esp-viewers": {},
	"users":       {},
	"user-detail": {},
	"esp-pager":   {},
	"streams":       {},
	"stream-edit":  {},
	"android-viewers": {},
	"viewer-detail": {},
}

func newAdminTemplates() (*adminTemplates, error) {
	funcMap := template.FuncMap{
		"macID":    macIDFromMAC,
		"safeHTML": func(s string) template.HTML { return template.HTML(s) },
	}

	pages := make(map[string]*template.Template, len(adminLibraryFor))
	for name, snippets := range adminLibraryFor {
		tmpl, err := template.New(name).Funcs(funcMap).ParseFS(
			templatesFS,
			"templates/admin/"+name+".html",
			"templates/admin/_nav.html",
			"templates/admin/_credentials_modal.html",
		)
		if err != nil {
			return nil, fmt.Errorf("parse admin shell %s: %w", name, err)
		}
		if err := addLibrarySnippets(tmpl, snippets); err != nil {
			return nil, fmt.Errorf("attach snippets to %s: %w", name, err)
		}
		pages[name] = tmpl
	}

	viewer := make(map[string]*template.Template, 2)
	homeShell, err := template.New("home").Funcs(funcMap).ParseFS(
		templatesFS,
		"templates/viewer/home.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse viewer home shell: %w", err)
	}
	if err := addLibrarySnippets(homeShell, []string{
		"intercom-idle.html",
		"intercom-ringing.html",
	}); err != nil {
		return nil, fmt.Errorf("attach viewer home snippets: %w", err)
	}
	viewer["home"] = homeShell

	loginShell, err := template.New("login").Funcs(funcMap).ParseFS(
		templatesFS,
		"templates/viewer/login.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse viewer login shell: %w", err)
	}
	viewer["login"] = loginShell

	settingsShell, err := template.New("settings").Funcs(funcMap).ParseFS(
		templatesFS,
		"templates/viewer/settings.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse viewer settings shell: %w", err)
	}
	viewer["settings"] = settingsShell

	return &adminTemplates{pages: pages, viewer: viewer}, nil
}

// addLibrarySnippets reads each library file out of the embedded
// design-library FS, strips the inline nav block, the demo
// `is-open` class and the doc comments, then registers the body
// as a named template inside tmpl.
func addLibrarySnippets(tmpl *template.Template, names []string) error {
	for _, s := range names {
		body, err := designLibraryFS.ReadFile("design-library/" + s)
		if err != nil {
			return fmt.Errorf("read %s: %w", s, err)
		}
		clean := htmlCommentRE.ReplaceAllString(string(body), "")
		clean = libraryDemoOpenRE.ReplaceAllString(clean, "")
		clean = libraryNavRE.ReplaceAllString(clean, "")
		if _, err := tmpl.New(s).Parse(clean); err != nil {
			return fmt.Errorf("parse %s: %w", s, err)
		}
	}
	return nil
}

// renderPage executes the named admin shell.
func (t *adminTemplates) renderPage(w io.Writer, name string, data any) error {
	tmpl, ok := t.pages[name]
	if !ok {
		return fmt.Errorf("unknown page template %q", name)
	}
	return tmpl.ExecuteTemplate(w, name+".html", data)
}

// renderPartial renders a single named template (typically a
// shared snippet like "credentials-modal") without the
// surrounding shell.
func (t *adminTemplates) renderPartial(w io.Writer, name string, data any) error {
	for _, tmpl := range t.pages {
		if lookup := tmpl.Lookup(name); lookup != nil {
			return lookup.Execute(w, data)
		}
	}
	return fmt.Errorf("unknown partial %q", name)
}

// renderViewer executes a viewer-side shell (login or home).
func (t *adminTemplates) renderViewer(w io.Writer, name string, data any) error {
	tmpl, ok := t.viewer[name]
	if !ok {
		return fmt.Errorf("unknown viewer template %q", name)
	}
	target := tmpl.Lookup(name + ".html")
	if target == nil {
		return fmt.Errorf("viewer template %s.html not in set", name)
	}
	return target.Execute(w, data)
}
