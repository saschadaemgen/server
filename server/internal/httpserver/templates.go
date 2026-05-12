package httpserver

import (
	"embed"
	"fmt"
	"html/template"
	"io"
)

//go:embed templates/*.html templates/admin/*.html templates/mieter/*.html
var templatesFS embed.FS

// adminTemplates bundles the admin-UI templates. Each page
// template is parsed together with the shared layout.html so
// {{template "content" .}} resolves to the page body and the
// `layout` block wraps it with nav + chrome.
//
// Partials (mocks_row, users_row, _error) are parsed separately
// because htmx requests render only the fragment without the
// layout wrapper.
//
// Tenant-facing pages live under templates/mieter/ and use a
// different look (no admin nav). They are parsed as standalone
// templates without the admin layout.
type adminTemplates struct {
	pages    map[string]*template.Template
	partials *template.Template
	mieter   map[string]*template.Template
}

func newAdminTemplates() (*adminTemplates, error) {
	funcMap := template.FuncMap{
		"icon": renderIcon,
	}

	pageNames := []string{"login", "dashboard", "settings", "mocks_list", "users_list"}
	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		tmpl, err := template.New(name).Funcs(funcMap).ParseFS(
			templatesFS,
			"templates/layout.html",
			"templates/admin/"+name+".html",
			"templates/admin/mocks_row.html",
			"templates/admin/users_row.html",
		)
		if err != nil {
			return nil, fmt.Errorf("parse admin page %s: %w", name, err)
		}
		pages[name] = tmpl
	}

	partials, err := template.New("partials").Funcs(funcMap).ParseFS(
		templatesFS,
		"templates/admin/mocks_row.html",
		"templates/admin/users_row.html",
		"templates/admin/_error.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse admin partials: %w", err)
	}

	mieterNames := []string{"home"}
	mieter := make(map[string]*template.Template, len(mieterNames))
	for _, name := range mieterNames {
		tmpl, err := template.New("mieter_"+name).Funcs(funcMap).ParseFS(
			templatesFS,
			"templates/mieter/"+name+".html",
		)
		if err != nil {
			return nil, fmt.Errorf("parse mieter page %s: %w", name, err)
		}
		mieter[name] = tmpl
	}

	return &adminTemplates{pages: pages, partials: partials, mieter: mieter}, nil
}

// renderPage executes the named page template against the
// shared layout.
func (t *adminTemplates) renderPage(w io.Writer, name string, data any) error {
	tmpl, ok := t.pages[name]
	if !ok {
		return fmt.Errorf("unknown page template %q", name)
	}
	return tmpl.ExecuteTemplate(w, "layout", data)
}

// renderPartial executes a single fragment template (e.g. one
// mock row or an error blurb) without the layout.
func (t *adminTemplates) renderPartial(w io.Writer, name string, data any) error {
	return t.partials.ExecuteTemplate(w, name, data)
}

// renderMieter executes a tenant-facing page. These pages carry
// their own <head>/<body> so no layout wrapping is applied.
func (t *adminTemplates) renderMieter(w io.Writer, name string, data any) error {
	tmpl, ok := t.mieter[name]
	if !ok {
		return fmt.Errorf("unknown mieter template %q", name)
	}
	return tmpl.ExecuteTemplate(w, "mieter_"+name, data)
}
