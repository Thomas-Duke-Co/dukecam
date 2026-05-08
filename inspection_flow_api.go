package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

// ─── Inspector-Facing API (JSON — consumed by HTMX + cached in IndexedDB) ──

// GET /api/inspections/templates — list active templates available for new inspections.
// Returns JSON array of active templates with category/item counts.
func (a *App) ListAvailableTemplates(c echo.Context) error {
	ctx := c.Request().Context()

	templates, err := a.db.ListActiveInspectionTemplates(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to load templates")
	}

	// Return empty array instead of null when no templates exist
	if templates == nil {
		templates = []ActiveInspectionTemplate{}
	}

	return c.JSON(http.StatusOK, templates)
}

// GET /api/inspections/templates/:id — retrieve a template's full checklist (categories + items).
// Used to populate the inspection checklist UI and for IndexedDB offline caching.
func (a *App) GetTemplateChecklist(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid template id")
	}

	tmpl, err := a.db.GetInspectionTemplateChecklist(ctx, id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "template not found or inactive")
	}

	// Ensure categories is always an array (never null)
	if tmpl.Categories == nil {
		tmpl.Categories = []InspectionTemplateCategory{}
	}
	for i := range tmpl.Categories {
		if tmpl.Categories[i].Items == nil {
			tmpl.Categories[i].Items = []InspectionTemplateItem{}
		}
	}

	return c.JSON(http.StatusOK, tmpl)
}

// GET /api/inspections/templates/:id/preview — HTMX endpoint returning an HTML preview
// of a template's sections and items. Used in the new-inspection wizard so inspectors
// can see what a template contains before committing to it.
func (a *App) GetTemplatePreviewHTML(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.HTML(http.StatusOK, `<div class="text-sm text-red-500 text-center py-2">Invalid template</div>`)
	}

	tmpl, err := a.db.GetInspectionTemplateChecklist(ctx, id)
	if err != nil {
		log.Printf("template preview load error for id=%d: %v", id, err)
		return c.HTML(http.StatusOK, `<div class="text-sm text-gray-400 text-center py-2">Template not found</div>`)
	}

	if len(tmpl.Categories) == 0 {
		return c.HTML(http.StatusOK, `<div class="text-sm text-gray-400 text-center py-3">This template has no checklist items yet.</div>`)
	}

	// Build a compact preview of sections and items
	var sb strings.Builder
	sb.WriteString(`<div class="space-y-3">`)

	totalItems := 0
	for _, cat := range tmpl.Categories {
		totalItems += len(cat.Items)
		sb.WriteString(fmt.Sprintf(`<div>
			<div class="flex items-center gap-2 mb-1">
				<span class="text-xs font-bold text-duke-dark uppercase tracking-wide">%s</span>
				<span class="text-[10px] text-gray-400 bg-gray-100 rounded-full px-1.5 py-0.5">%d</span>
			</div>
			<div class="space-y-0.5 pl-2 border-l-2 border-gray-200">`,
			escapeHTML(cat.Name), len(cat.Items)))

		for _, item := range cat.Items {
			photoIcon := ""
			if item.RequirePhoto {
				photoIcon = `<svg class="w-3 h-3 text-gray-400 flex-shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 9a2 2 0 012-2h.93a2 2 0 001.664-.89l.812-1.22A2 2 0 0110.07 4h3.86a2 2 0 011.664.89l.812 1.22A2 2 0 0018.07 7H19a2 2 0 012 2v9a2 2 0 01-2 2H5a2 2 0 01-2-2V9z"/></svg>`
			}
			sb.WriteString(fmt.Sprintf(`<div class="flex items-center gap-1.5 text-xs text-gray-600 py-0.5">
				<span class="w-1 h-1 rounded-full bg-gray-300 flex-shrink-0"></span>
				<span class="truncate">%s</span>%s
			</div>`, escapeHTML(item.Label), photoIcon))
		}

		sb.WriteString(`</div></div>`)
	}

	sb.WriteString(fmt.Sprintf(`<div class="text-center text-xs text-gray-400 pt-1">%d categories · %d items total</div>`,
		len(tmpl.Categories), totalItems))
	sb.WriteString(`</div>`)

	return c.HTML(http.StatusOK, sb.String())
}

// GET /api/inspections/inspectors — list available inspectors from Fyxt.
// Returns JSON array of inspector users (name, email, id) for the inspector picker.
// Cached in-memory for 10 minutes; gracefully returns empty list if Fyxt is unavailable.
func (a *App) ListInspectors(c echo.Context) error {
	ctx := c.Request().Context()

	if !a.fyxt.IsConfigured() {
		return c.JSON(http.StatusOK, []interface{}{})
	}

	inspectors, err := a.fyxt.GetInspectors(ctx)
	if err != nil {
		c.Logger().Errorf("Fyxt inspector fetch failed: %v", err)
		// Return empty list rather than error — UI can show manual entry fallback
		return c.JSON(http.StatusOK, []interface{}{})
	}

	// Shape response for the UI (minimal payload for mobile)
	type inspectorResponse struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}

	result := make([]inspectorResponse, 0, len(inspectors))
	for _, u := range inspectors {
		result = append(result, inspectorResponse{
			ID:    u.ID,
			Name:  u.FullName(),
			Email: u.Email,
		})
	}

	return c.JSON(http.StatusOK, result)
}

// GET /api/inspections/inspectors/search?q=... — HTMX endpoint returning HTML fragment
// for the inspector search dropdown. Returns rendered list of matching inspectors.
func (a *App) SearchInspectors(c echo.Context) error {
	ctx := c.Request().Context()
	query := c.QueryParam("q")

	if !a.fyxt.IsConfigured() {
		// Return empty state with manual entry hint
		return c.HTML(http.StatusOK, `<div class="p-3 text-sm text-gray-500 text-center">Fyxt not configured — enter inspector name manually</div>`)
	}

	inspectors, err := a.fyxt.SearchInspectors(ctx, query)
	if err != nil {
		log.Printf("Fyxt inspector search error: %v", err)
		return c.HTML(http.StatusOK, `<div class="p-3 text-sm text-red-500 text-center">Unable to load inspectors</div>`)
	}

	if len(inspectors) == 0 {
		msg := "No inspectors found"
		if query != "" {
			msg = "No inspectors matching \"" + query + "\""
		}
		return c.HTML(http.StatusOK, `<div class="p-3 text-sm text-gray-500 text-center">`+msg+`</div>`)
	}

	// Render the inspector-results fragment
	renderer := c.Echo().Renderer.(*Renderer)
	var buf bytes.Buffer
	if err := renderer.RenderFragment(&buf, "inspector-results", inspectors); err != nil {
		log.Printf("inspector-results render error: %v", err)
		return c.HTML(http.StatusOK, `<div class="p-3 text-sm text-red-500 text-center">Render error</div>`)
	}

	return c.HTML(http.StatusOK, buf.String())
}
