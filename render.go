package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// Renderer implements echo.Renderer with Go html/template.
type Renderer struct {
	templates map[string]*template.Template
}

func NewRenderer() *Renderer {
	funcMap := template.FuncMap{
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		"derefInt": func(i *int) int {
			if i == nil {
				return 0
			}
			return *i
		},
		"formatDate": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.Format("Monday, January 2, 2006")
		},
		"formatDateTime": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.Format("Jan 2, 2006 3:04 PM")
		},
		"formatDateKey": func(t time.Time) string {
			return t.Format("2006-01-02")
		},
		"formatDateLabel": func(t time.Time) string {
			return t.Format("Monday, January 2, 2006")
		},
		"formatTime": func(t time.Time) string {
			return t.Format("Jan 2, 2006 3:04 PM")
		},
		"json": func(v interface{}) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"add": func(a, b int) int {
			return a + b
		},
		"initials": func(name string) string {
			parts := strings.Fields(name)
			if len(parts) == 0 {
				return "?"
			}
			result := string([]rune(parts[0])[0:1])
			if len(parts) > 1 {
				result += string([]rune(parts[len(parts)-1])[0:1])
			}
			return strings.ToUpper(result)
		},
		"safeHTML": func(s string) template.HTML {
			return template.HTML(s)
		},
	}

	r := &Renderer{templates: make(map[string]*template.Template)}

	// Pages that don't need fragments
	for _, page := range []string{"home", "project", "share", "offline"} {
		t := template.Must(
			template.New("").Funcs(funcMap).ParseFiles(
				"templates/base.html",
				fmt.Sprintf("templates/%s.html", page),
			),
		)
		r.templates[page] = t
	}

	// Admin needs fragments (for project-item, worker-item templates used inline)
	r.templates["admin"] = template.Must(
		template.New("").Funcs(funcMap).ParseFiles(
			"templates/base.html",
			"templates/admin.html",
			"templates/fragments.html",
		),
	)

	// Inspection admin pages (need inspection fragments)
	r.templates["admin_inspections"] = template.Must(
		template.New("").Funcs(funcMap).ParseFiles(
			"templates/base.html",
			"templates/admin_inspections.html",
			"templates/inspection_fragments.html",
		),
	)
	r.templates["admin_inspection_detail"] = template.Must(
		template.New("").Funcs(funcMap).ParseFiles(
			"templates/base.html",
			"templates/admin_inspection_detail.html",
			"templates/inspection_fragments.html",
		),
	)

	// New inspection wizard (template + property + inspector selection)
	r.templates["inspection_new"] = template.Must(
		template.New("").Funcs(funcMap).ParseFiles(
			"templates/base.html",
			"templates/inspection_new.html",
			"templates/inspection_fragments.html",
		),
	)

	// Inspection conduct page (live checklist with pass/fail/needs-attention)
	r.templates["inspection_conduct"] = template.Must(
		template.New("").Funcs(funcMap).ParseFiles(
			"templates/base.html",
			"templates/inspection_conduct.html",
		),
	)

	// Inspection share page (public read-only view)
	r.templates["inspection_share"] = template.Must(
		template.New("").Funcs(funcMap).ParseFiles(
			"templates/base.html",
			"templates/inspection_share.html",
		),
	)

	// Fragment templates for HTMX partial responses
	r.templates["fragments"] = template.Must(
		template.New("").Funcs(funcMap).ParseFiles("templates/fragments.html"),
	)

	// Inspection fragment templates for HTMX partial responses
	r.templates["inspection_fragments"] = template.Must(
		template.New("").Funcs(funcMap).ParseFiles("templates/inspection_fragments.html"),
	)

	return r
}

func (r *Renderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	tmpl, ok := r.templates[name]
	if !ok {
		return fmt.Errorf("template %q not found", name)
	}
	return tmpl.ExecuteTemplate(w, "base", data)
}

// RenderFragment renders a named fragment template (for HTMX responses).
// It checks both the main fragments and inspection fragments.
func (r *Renderer) RenderFragment(w io.Writer, name string, data interface{}) error {
	// Try main fragments first
	if tmpl, ok := r.templates["fragments"]; ok {
		if t := tmpl.Lookup(name); t != nil {
			return t.Execute(w, data)
		}
	}
	// Try inspection fragments
	if tmpl, ok := r.templates["inspection_fragments"]; ok {
		if t := tmpl.Lookup(name); t != nil {
			return t.Execute(w, data)
		}
	}
	return fmt.Errorf("fragment %q not found", name)
}
