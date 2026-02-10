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
	for _, page := range []string{"home", "project", "share"} {
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

	// Fragment templates for HTMX partial responses
	r.templates["fragments"] = template.Must(
		template.New("").Funcs(funcMap).ParseFiles("templates/fragments.html"),
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
func (r *Renderer) RenderFragment(w io.Writer, name string, data interface{}) error {
	tmpl, ok := r.templates["fragments"]
	if !ok {
		return fmt.Errorf("fragments template not found")
	}
	return tmpl.ExecuteTemplate(w, name, data)
}
