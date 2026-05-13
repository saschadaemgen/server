package httpserver

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"strings"
)

// htmlCommentRE matches HTML comment blocks. The Claude-Design
// library files start with extensive doc comments that contain
// example {{template "..." .}} action snippets. Go's
// html/template parses those actions even inside <!-- ... -->,
// so we strip the comments before handing the body to Parse
// (saving a "no such template" runtime error from a phantom
// reference). Production rendering does not need the comments.
var htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)

//go:embed templates/admin/*.html templates/mieter/*.html
var templatesFS embed.FS

// adminTemplates bundles every page template the http handlers
// can render. Each entry is a complete html/template tree that
// can run on its own via ExecuteTemplate(w, name, data).
//
// Saison 13-02-FIX3: the Claude-Design library now provides the
// page-level HTML for every screen. The files in templates/...
// are thin layout-shells that include the library snippets via
// {{template "snippet-name.html" .}}. The CSS/JS/library shells
// are wired in static.go via the same embed FS that this file
// reads its snippets from.
type adminTemplates struct {
	pages  map[string]*template.Template
	mieter map[string]*template.Template
}

// macIDFromMAC turns a colon-separated MAC into a CSS-safe
// suffix for HTML id attributes. Kept for callers that still
// need the helper.
func macIDFromMAC(mac string) string {
	return strings.ToLower(strings.ReplaceAll(mac, ":", ""))
}

// adminLibraryFor lists the design-library snippets each admin
// page shell needs. login is the only page that does not embed
// the magic-link modal.
var adminLibraryFor = map[string][]string{
	"login":     {"admin-login.html"},
	"dashboard": {"admin-dashboard.html", "modal-magic-link.html"},
	"users":     {"admin-users.html", "modal-magic-link.html"},
	"mocks":     {"admin-mocks.html", "modal-magic-link.html"},
	"settings":  {"admin-settings.html", "modal-magic-link.html"},
}

func newAdminTemplates() (*adminTemplates, error) {
	funcMap := template.FuncMap{
		"macID": macIDFromMAC,
	}

	pages := make(map[string]*template.Template, len(adminLibraryFor))
	for name, snippets := range adminLibraryFor {
		tmpl, err := template.New(name).Funcs(funcMap).ParseFS(
			templatesFS,
			"templates/admin/"+name+".html",
		)
		if err != nil {
			return nil, fmt.Errorf("parse admin shell %s: %w", name, err)
		}
		if err := addLibrarySnippets(tmpl, snippets); err != nil {
			return nil, fmt.Errorf("attach snippets to %s: %w", name, err)
		}
		pages[name] = tmpl
	}

	mieter := make(map[string]*template.Template, 1)
	homeShell, err := template.New("home").Funcs(funcMap).ParseFS(
		templatesFS,
		"templates/mieter/home.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse mieter shell: %w", err)
	}
	if err := addLibrarySnippets(homeShell, []string{
		"intercom-idle.html",
		"intercom-history.html",
		"intercom-ringing.html",
	}); err != nil {
		return nil, fmt.Errorf("attach mieter snippets: %w", err)
	}
	mieter["home"] = homeShell

	return &adminTemplates{pages: pages, mieter: mieter}, nil
}

// addLibrarySnippets reads each library file out of the embedded
// design-library FS and registers it as a named template inside
// tmpl. The template name is the file basename so the shell can
// pull it in with `{{template "intercom-idle.html" .}}`.
func addLibrarySnippets(tmpl *template.Template, names []string) error {
	for _, s := range names {
		body, err := designLibraryFS.ReadFile("design-library/" + s)
		if err != nil {
			return fmt.Errorf("read %s: %w", s, err)
		}
		clean := htmlCommentRE.ReplaceAllString(string(body), "")
		if _, err := tmpl.New(s).Parse(clean); err != nil {
			return fmt.Errorf("parse %s: %w", s, err)
		}
	}
	return nil
}

// renderPage executes the named admin shell. The shell pulls in
// the page-specific Claude-Design snippet via {{template ...}}.
func (t *adminTemplates) renderPage(w io.Writer, name string, data any) error {
	tmpl, ok := t.pages[name]
	if !ok {
		return fmt.Errorf("unknown page template %q", name)
	}
	return tmpl.ExecuteTemplate(w, name+".html", data)
}

// renderPartial renders a single named template (typically a
// library snippet) without the surrounding shell. Looked up
// inside whichever admin page has it parsed.
func (t *adminTemplates) renderPartial(w io.Writer, name string, data any) error {
	for _, tmpl := range t.pages {
		if lookup := tmpl.Lookup(name); lookup != nil {
			return lookup.Execute(w, data)
		}
	}
	return fmt.Errorf("unknown partial %q", name)
}

// renderMieter executes the tenant-facing home shell.
func (t *adminTemplates) renderMieter(w io.Writer, name string, data any) error {
	tmpl, ok := t.mieter[name]
	if !ok {
		return fmt.Errorf("unknown mieter template %q", name)
	}
	target := tmpl.Lookup(name + ".html")
	if target == nil {
		names := make([]string, 0)
		for _, sub := range tmpl.Templates() {
			names = append(names, sub.Name())
		}
		return fmt.Errorf("mieter template %s.html not in set; have: %v", name, names)
	}
	return target.Execute(w, data)
}
