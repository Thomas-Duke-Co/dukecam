package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

// listInspectionSummaries fetches recent inspections for the home page.
// Returns an empty slice if the inspections table doesn't exist yet.
func (a *App) listInspectionSummaries(ctx context.Context) []InspectionSummary {
	query := `
		SELECT i.id,
		       COALESCE(i.property_name, 'Unknown Property'),
		       COALESCE(t.name, 'General Inspection'),
		       COALESCE(i.inspector_name, ''),
		       COALESCE(i.status, 'draft'),
		       COALESCE(
		           (SELECT COUNT(*) FROM inspection_responses r WHERE r.inspection_id = i.id AND r.status IS NOT NULL),
		           0
		       ),
		       COALESCE(
		           (SELECT COUNT(*) FROM inspection_responses r WHERE r.inspection_id = i.id),
		           0
		       )
		FROM inspections i
		LEFT JOIN inspection_templates t ON t.id = i.template_id
		ORDER BY i.updated_at DESC NULLS LAST, i.created_at DESC
		LIMIT 50
	`

	rows, err := a.db.pool.Query(ctx, query)
	if err != nil {
		// Table may not exist yet — that's fine
		log.Printf("inspections query (may not exist yet): %v", err)
		return nil
	}
	defer rows.Close()

	var results []InspectionSummary
	for rows.Next() {
		var s InspectionSummary
		if err := rows.Scan(&s.ID, &s.PropertyName, &s.TemplateName, &s.InspectorName, &s.Status, &s.CompletedCount, &s.TotalCount); err != nil {
			log.Printf("inspection scan error: %v", err)
			continue
		}
		// Compute progress percentage
		if s.TotalCount > 0 {
			s.ProgressPct = (s.CompletedCount * 100) / s.TotalCount
		}
		// Human-readable status
		switch s.Status {
		case "completed":
			s.StatusLabel = "Completed"
		case "in_progress":
			s.StatusLabel = "In Progress"
		default:
			s.StatusLabel = "Draft"
		}
		results = append(results, s)
	}

	return results
}

// ─── New Inspection Page ────────────────────────────────────────

// GET /inspections/new — template selection + property selection wizard
func (a *App) NewInspectionPage(c echo.Context) error {
	ctx := c.Request().Context()

	// Load active templates with category/item counts for the picker cards
	templates, err := a.db.ListActiveInspectionTemplates(ctx)
	if err != nil {
		log.Printf("failed to load active inspection templates: %v", err)
		templates = nil
	}

	return c.Render(http.StatusOK, "inspection_new", map[string]interface{}{
		"Templates": templates,
	})
}

// ─── HTMX Property Search Endpoint ──────────────────────────────

// GET /api/inspections/properties/search?q=... — HTMX endpoint returning HTML property cards.
// Used by the new-inspection wizard's property search step.
func (a *App) SearchPropertiesHTML(c echo.Context) error {
	ctx := c.Request().Context()
	q := strings.TrimSpace(c.QueryParam("q"))

	if !a.propertyOS.IsConfigured() {
		return c.HTML(http.StatusOK, `
			<div class="text-center py-8 text-gray-400">
				<p class="text-sm">PropertyOS not configured</p>
				<p class="text-xs mt-1">Set PROPERTYOS_URL to enable property search</p>
			</div>
		`)
	}

	allBuildings, err := a.propertyOS.ListBuildings(ctx)
	if err != nil {
		log.Printf("PropertyOS fetch error: %v", err)
		return c.HTML(http.StatusOK, `
			<div class="text-center py-8 text-gray-400">
				<p class="text-sm">Could not reach PropertyOS</p>
				<p class="text-xs mt-1">Properties will be available when the service is online</p>
			</div>
		`)
	}

	// Filter by search query (case-insensitive name/address match) and active status
	var filtered []Building
	for _, b := range allBuildings {
		if !b.Active {
			continue
		}
		if q == "" ||
			strings.Contains(strings.ToLower(b.Name), strings.ToLower(q)) ||
			strings.Contains(strings.ToLower(b.Address), strings.ToLower(q)) ||
			strings.Contains(strings.ToLower(b.City), strings.ToLower(q)) {
			filtered = append(filtered, b)
		}
	}

	if len(filtered) == 0 {
		if q == "" {
			return c.HTML(http.StatusOK, `
				<div class="text-center py-8 text-gray-400">
					<svg class="w-10 h-10 mx-auto mb-2 text-gray-300" fill="none" stroke="currentColor" viewBox="0 0 24 24">
						<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 21V5a2 2 0 00-2-2H7a2 2 0 00-2 2v16m14 0h2m-2 0h-5m-9 0H3m2 0h5M9 7h1m-1 4h1m4-4h1m-1 4h1m-5 10v-5a1 1 0 011-1h2a1 1 0 011 1v5m-4 0h4"/>
					</svg>
					<p class="text-sm">No properties available</p>
				</div>
			`)
		}
		return c.HTML(http.StatusOK, fmt.Sprintf(`
			<div class="text-center py-6 text-gray-400">
				<p class="text-sm">No properties matching &ldquo;%s&rdquo;</p>
			</div>
		`, escapeHTML(q)))
	}

	// Render as tappable cards
	var sb strings.Builder
	for _, b := range filtered {
		addr := b.Address
		if b.City != "" {
			addr += ", " + b.City
		}
		if b.State != "" {
			addr += ", " + b.State
		}
		sb.WriteString(fmt.Sprintf(`
		<button onclick="selectProperty(%d, '%s', %d, 0, '%s')"
				class="w-full text-left bg-white border border-gray-200 rounded-xl p-4 hover:border-duke-teal hover:shadow-sm transition-all active:scale-[0.98]">
			<div class="flex items-center gap-3">
				<div class="w-10 h-10 rounded-lg bg-duke-teal/10 flex items-center justify-center flex-shrink-0">
					<svg class="w-5 h-5 text-duke-teal" fill="none" stroke="currentColor" viewBox="0 0 24 24">
						<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 21V5a2 2 0 00-2-2H7a2 2 0 00-2 2v16m14 0h2m-2 0h-5m-9 0H3m2 0h5M9 7h1m-1 4h1m4-4h1m-1 4h1m-5 10v-5a1 1 0 011-1h2a1 1 0 011 1v5m-4 0h4"/>
					</svg>
				</div>
				<div class="flex-1 min-w-0">
					<div class="font-semibold text-duke-dark text-sm">%s</div>
					<div class="text-xs text-gray-400">%s</div>
				</div>
				<svg class="w-5 h-5 text-gray-300 flex-shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7"/></svg>
			</div>
		</button>`,
			b.ID,
			escapeAttr(b.Name),
			b.ID,
			escapeAttr(addr),
			escapeHTML(b.Name),
			escapeHTML(addr),
		))
	}

	return c.HTML(http.StatusOK, sb.String())
}

// ─── HTMX Unit/Suite Dropdown Endpoint ───────────────────────────

// GET /api/inspections/properties/:id/units — HTMX endpoint returning HTML unit/suite options.
// Called after a property is selected; returns a dropdown of available units from PropertyOS rent roll.
func (a *App) GetPropertyUnitsHTML(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.HTML(http.StatusOK, `<option value="">Invalid property</option>`)
	}

	if !a.propertyOS.IsConfigured() {
		return c.HTML(http.StatusOK, `<option value="">PropertyOS not configured</option>`)
	}

	detail, err := a.propertyOS.GetBuilding(ctx, id)
	if err != nil {
		log.Printf("PropertyOS building detail error for units: %v", err)
		return c.HTML(http.StatusOK, `<option value="">Could not load units</option>`)
	}

	if len(detail.RentRoll) == 0 {
		return c.HTML(http.StatusOK, `<option value="">No units found</option>`)
	}

	// Build the HTML options — "Entire property" first, then each unit
	var sb strings.Builder
	sb.WriteString(`<option value="0">Entire Property</option>`)

	for _, row := range detail.RentRoll {
		label := row.Suite
		if label == "" {
			label = fmt.Sprintf("Unit %d", row.UnitID)
		}
		// Add tenant name if occupied
		if row.TenantName != nil && *row.TenantName != "" {
			label += " — " + *row.TenantName
		}
		sb.WriteString(fmt.Sprintf(`<option value="%d" data-suite="%s">%s</option>`,
			row.UnitID,
			escapeAttr(row.Suite),
			escapeHTML(label),
		))
	}

	return c.HTML(http.StatusOK, sb.String())
}

// ─── HTMX Inspection List Filter by Property ────────────────────

// GET /api/inspections/filter?property_id=... — HTMX endpoint returning filtered inspection list HTML.
// Used by the property dropdown on the Inspections tab to filter the inspection list.
func (a *App) FilterInspectionsHTML(c echo.Context) error {
	ctx := c.Request().Context()

	propertyID := c.QueryParam("property_id")

	// Fetch all inspections (reuse existing summary logic)
	all := a.listInspectionSummaries(ctx)

	// If a property_id is specified and not "all", filter by property name match
	var filtered []InspectionSummary
	if propertyID == "" || propertyID == "all" {
		filtered = all
	} else {
		// Look up the property name from PropertyOS to match against inspection property_name
		pid, err := strconv.Atoi(propertyID)
		if err != nil {
			filtered = all
		} else {
			// Get property name for matching
			var propertyName string
			if a.propertyOS.IsConfigured() {
				buildings, _ := a.propertyOS.ListBuildings(ctx)
				for _, b := range buildings {
					if b.ID == pid {
						propertyName = b.Name
						break
					}
				}
			}

			if propertyName == "" {
				filtered = all
			} else {
				for _, s := range all {
					if strings.EqualFold(s.PropertyName, propertyName) {
						filtered = append(filtered, s)
					}
				}
			}
		}
	}

	// Render the inspection list HTML
	var sb strings.Builder

	if len(filtered) == 0 {
		sb.WriteString(`<div class="text-center py-12 text-gray-500">`)
		sb.WriteString(`<div class="text-4xl mb-3">📋</div>`)
		if propertyID != "" && propertyID != "all" {
			sb.WriteString(`<p class="text-sm">No inspections for this property yet.</p>`)
		} else {
			sb.WriteString(`<p class="text-lg">No inspections yet.</p>`)
			sb.WriteString(`<p class="mt-2 text-sm">Start a new inspection to get going.</p>`)
		}
		sb.WriteString(`</div>`)
		return c.HTML(http.StatusOK, sb.String())
	}

	sb.WriteString(`<div class="divide-y divide-gray-200">`)
	for _, insp := range filtered {
		// Status icon classes and SVG
		var statusBgClass, statusSVG string
		switch insp.Status {
		case "completed":
			statusBgClass = "bg-green-100 text-green-600"
			statusSVG = `<svg class="w-7 h-7" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z"/></svg>`
		case "in_progress":
			statusBgClass = "bg-yellow-100 text-yellow-600"
			statusSVG = `<svg class="w-7 h-7" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 8v4l3 3m6-3a9 9 0 11-18 0 9 9 0 0118 0z"/></svg>`
		default:
			statusBgClass = "bg-gray-100 text-gray-400"
			statusSVG = `<svg class="w-7 h-7" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2"/></svg>`
		}

		progressBarClass := "bg-duke-teal"
		if insp.Status == "completed" {
			progressBarClass = "bg-green-500"
		}

		sb.WriteString(fmt.Sprintf(`
		<a href="/inspection/%d" class="flex items-center gap-4 px-5 py-4 bg-white hover:bg-gray-50 transition-colors no-underline text-inherit">
			<div class="w-14 h-14 min-w-[3.5rem] rounded-lg flex items-center justify-center flex-shrink-0 %s">%s</div>
			<div class="flex-1 min-w-0">
				<div class="text-[17px] font-bold text-duke-dark leading-snug">%s</div>
				<div class="text-sm text-gray-500 leading-snug">%s</div>
				<div class="text-xs text-gray-400 mt-0.5">%s%s</div>
			</div>
			<div class="flex-shrink-0 text-center w-16">
				<div class="text-[11px] text-gray-500 uppercase tracking-wide">Done</div>
				<div class="text-lg font-bold text-duke-dark">%d/%d</div>
				<div class="w-full bg-gray-200 rounded-full h-1.5 mt-1">
					<div class="h-1.5 rounded-full %s" style="width: %d%%"></div>
				</div>
			</div>
			<div class="flex-shrink-0 text-gray-300">
				<svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7"/></svg>
			</div>
		</a>`,
			insp.ID,
			statusBgClass, statusSVG,
			escapeHTML(insp.PropertyName),
			escapeHTML(insp.TemplateName),
			func() string {
				if insp.InspectorName != "" {
					return "by " + escapeHTML(insp.InspectorName) + " · "
				}
				return ""
			}(),
			escapeHTML(insp.StatusLabel),
			insp.CompletedCount, insp.TotalCount,
			progressBarClass, insp.ProgressPct,
		))
	}
	sb.WriteString(`</div>`)

	return c.HTML(http.StatusOK, sb.String())
}

// escapeHTML does minimal HTML entity escaping for text content.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// escapeAttr escapes a string for use in HTML attribute values (onclick, etc.).
func escapeAttr(s string) string {
	s = escapeHTML(s)
	s = strings.ReplaceAll(s, "'", "&#39;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
