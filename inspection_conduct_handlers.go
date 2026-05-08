package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

// ─── Create Inspection (from wizard) ────────────────────────────

// POST /api/inspections/create — creates a new inspection and redirects to the conduct page.
func (a *App) CreateInspectionHandler(c echo.Context) error {
	ctx := c.Request().Context()

	templateIDStr := c.FormValue("template_id")
	propertyIDStr := c.FormValue("property_id")
	propertyName := c.FormValue("property_name")
	buildingIDStr := c.FormValue("building_id")
	unitIDStr := c.FormValue("unit_id")
	notes := c.FormValue("notes")

	propertyID, _ := strconv.Atoi(propertyIDStr)

	var templateID *int
	if tid, err := strconv.Atoi(templateIDStr); err == nil && tid > 0 {
		templateID = &tid
	}

	var buildingID *int
	if bid, err := strconv.Atoi(buildingIDStr); err == nil && bid > 0 {
		buildingID = &bid
	}

	var unitID *int
	if uid, err := strconv.Atoi(unitIDStr); err == nil && uid > 0 {
		unitID = &uid
	}

	var notesPtr *string
	if strings.TrimSpace(notes) != "" {
		notesPtr = &notes
	}

	// Inspector identity from the inspector picker (Fyxt integration)
	inspectorID := strings.TrimSpace(c.FormValue("inspector_id"))
	inspectorName := strings.TrimSpace(c.FormValue("inspector_name"))
	if inspectorName == "" {
		inspectorName = "Inspector" // fallback if picker wasn't used
	}

	var inspectorIDPtr *string
	if inspectorID != "" {
		inspectorIDPtr = &inspectorID
	}

	insp, err := a.db.CreateInspection(ctx, templateID, propertyID, propertyName,
		buildingID, unitID, inspectorIDPtr, inspectorName, notesPtr)
	if err != nil {
		log.Printf("create inspection error: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to create inspection")
	}

	// HTMX redirect to the conduct page
	c.Response().Header().Set("HX-Redirect", fmt.Sprintf("/inspection/%d", insp.ID))
	return c.NoContent(http.StatusOK)
}

// ─── Inspection Conduct Page ────────────────────────────────────

// RenderedCategory holds a category name + pre-rendered HTML for its items.
type RenderedCategory struct {
	Name      string
	ItemCount int
	ItemsHTML string
}

// GET /inspection/:id — the live checklist page where inspectors mark items.
func (a *App) InspectionConductPage(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	checklist, err := a.db.GetInspectionChecklist(ctx, id)
	if err != nil {
		log.Printf("load inspection checklist error: %v", err)
		return echo.NewHTTPError(http.StatusNotFound, "Inspection not found")
	}

	// Load all photos for this inspection (grouped by item type + ID)
	photosByItem, err := a.db.GetAllInspectionPhotos(ctx, id)
	if err != nil {
		log.Printf("load inspection photos error: %v", err)
		photosByItem = &PhotosByItemResult{
			ByItemID:      make(map[int][]InspectionPhoto),
			ByAdhocItemID: make(map[int][]InspectionPhoto),
		}
	}

	// Pre-render item HTML for each category (avoids complex template functions)
	var rendered []RenderedCategory
	for _, cat := range checklist.Categories {
		var sb strings.Builder
		for _, item := range cat.Items {
			var photos []InspectionPhoto
			if item.IsAdhoc {
				photos = photosByItem.ByAdhocItemID[item.ItemID]
			} else {
				photos = photosByItem.ByItemID[item.ItemID]
			}
			sb.WriteString(renderChecklistItemHTML(checklist.Inspection.ID, item, photos))
		}
		rendered = append(rendered, RenderedCategory{
			Name:      cat.Name,
			ItemCount: len(cat.Items),
			ItemsHTML: sb.String(),
		})
	}

	// Collect labels of flagged items for the "Create Work Order" pre-fill.
	var flaggedLabels []string
	for _, cat := range checklist.Categories {
		for _, item := range cat.Items {
			if item.Status != nil && (*item.Status == ItemStatusFail || *item.Status == ItemStatusNeedsAttention) {
				flaggedLabels = append(flaggedLabels, item.Label)
			}
		}
	}

	return c.Render(http.StatusOK, "inspection_conduct", map[string]interface{}{
		"Inspection":         checklist.Inspection,
		"Categories":         checklist.Categories,
		"RenderedCategories": rendered,
		"Stats":              checklist.Stats,
		"FlaggedItems":       flaggedLabels,
	})
}

// ─── Update Checklist Item Status (HTMX) ────────────────────────

// POST /api/inspections/:id/item/:itemId/status — set pass/fail/needs-attention on a checklist item.
// Returns an HTMX fragment with the updated item row + progress bar.
func (a *App) UpdateItemStatusHandler(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	itemID, err := strconv.Atoi(c.Param("itemId"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid item id")
	}

	statusStr := c.FormValue("status")
	status := ItemStatus(statusStr)

	// Handle "clear" action — toggle off if same status
	if statusStr == "clear" {
		if err := a.db.ClearInspectionItemStatus(ctx, inspectionID, itemID); err != nil {
			log.Printf("clear item status error: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to clear status")
		}
	} else {
		if !ValidItemStatuses[status] {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid status: must be pass, fail, or needs_attention")
		}

		var notesPtr *string
		if n := strings.TrimSpace(c.FormValue("notes")); n != "" {
			notesPtr = &n
		}

		if _, err := a.db.UpdateInspectionItemStatus(ctx, inspectionID, itemID, status, notesPtr); err != nil {
			log.Printf("update item status error: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to update status")
		}
	}

	// Return updated stats + item row as HTML fragment
	stats, _ := a.db.GetInspectionStats(ctx, inspectionID)

	progressHTML := renderProgressBarHTML(stats)

	// Build the updated item row
	checklist, err := a.db.GetInspectionChecklist(ctx, inspectionID)
	if err != nil {
		return c.HTML(http.StatusOK, progressHTML)
	}

	// Load photos for this item so they persist across status updates
	itemPhotos, _ := a.db.GetInspectionPhotosByItem(ctx, inspectionID, itemID)

	// Find the template item in the checklist (skip ad-hoc items to avoid ID collision)
	var itemHTML string
	for _, cat := range checklist.Categories {
		for _, item := range cat.Items {
			if item.ItemID == itemID && !item.IsAdhoc {
				itemHTML = renderChecklistItemHTML(inspectionID, item, itemPhotos)
				break
			}
		}
	}

	return c.HTML(http.StatusOK, itemHTML+progressHTML)
}

// ─── Complete Inspection ────────────────────────────────────────

// POST /api/inspections/:id/complete — marks the inspection as completed.
func (a *App) CompleteInspectionHandler(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	_, err = a.db.CompleteInspection(ctx, id)
	if err != nil {
		log.Printf("complete inspection error: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to complete inspection")
	}

	c.Response().Header().Set("HX-Redirect", "/?tab=inspections")
	return c.NoContent(http.StatusOK)
}

// ─── Submit Full Inspection (offline sync) ──────────────────────

// POST /api/inspections/submit — accepts a complete inspection with all responses in one request.
// This is the primary sync endpoint for offline-capable inspections. The client queues the
// entire inspection in IndexedDB and POSTs it here when connectivity is restored.
//
// Request body (JSON):
//
//	{
//	  "template_id": 1,
//	  "property_id": 42,
//	  "property_name": "1700 W Big Beaver",
//	  "building_id": 5,
//	  "unit_id": 12,
//	  "inspector_id": "fyxt-uuid",
//	  "inspector_name": "Jane Smith",
//	  "notes": "Annual walkthrough",
//	  "complete": true,
//	  "responses": [
//	    {"item_id": 1, "status": "pass"},
//	    {"item_id": 2, "status": "fail", "notes": "Cracked sidewalk near entrance"}
//	  ],
//	  "adhoc_items": [
//	    {"label": "Broken window latch", "status": "fail", "category_name": "Ad-hoc Items"}
//	  ]
//	}
//
// Returns JSON with the created inspection record, or an error.
func (a *App) SubmitInspectionHandler(c echo.Context) error {
	ctx := c.Request().Context()

	var sub InspectionSubmission
	if err := c.Bind(&sub); err != nil {
		log.Printf("submit inspection bind error: %v", err)
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid request body: " + err.Error(),
		})
	}

	// Validate required fields
	if sub.PropertyName == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "property_name is required",
		})
	}
	if sub.InspectorName == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "inspector_name is required",
		})
	}

	// Validate response statuses
	for i, resp := range sub.Responses {
		if !ValidItemStatuses[resp.Status] {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("invalid status %q for response[%d]", resp.Status, i),
			})
		}
		if resp.ItemID <= 0 {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("invalid item_id for response[%d]", i),
			})
		}
	}

	// Validate adhoc items
	for i, adhoc := range sub.AdhocItems {
		if strings.TrimSpace(adhoc.Label) == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("label is required for adhoc_items[%d]", i),
			})
		}
		if adhoc.Status != nil && !ValidItemStatuses[*adhoc.Status] {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("invalid status %q for adhoc_items[%d]", *adhoc.Status, i),
			})
		}
	}

	insp, err := a.db.SubmitInspection(ctx, sub)
	if err != nil {
		log.Printf("submit inspection error: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "Failed to save inspection: " + err.Error(),
		})
	}

	log.Printf("inspection %d submitted (status=%s, %d responses, %d adhoc items)",
		insp.ID, insp.Status, len(sub.Responses), len(sub.AdhocItems))

	return c.JSON(http.StatusCreated, map[string]interface{}{
		"inspection": insp,
		"message":    "Inspection saved successfully",
	})
}

// ─── Ad-hoc Item Handlers ───────────────────────────────────────

// POST /api/inspections/:id/adhoc — adds an ad-hoc checklist item to a live inspection.
// Returns the new item row + updated progress bar via HTMX.
func (a *App) CreateAdhocItemHandler(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	label := strings.TrimSpace(c.FormValue("label"))
	if label == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "label is required")
	}

	categoryName := strings.TrimSpace(c.FormValue("category_name"))
	if categoryName == "" {
		categoryName = "Ad-hoc Items"
	}

	var descPtr *string
	if d := strings.TrimSpace(c.FormValue("description")); d != "" {
		descPtr = &d
	}

	item, err := a.db.CreateAdhocItem(ctx, inspectionID, label, categoryName, descPtr)
	if err != nil {
		log.Printf("create adhoc item error: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to add item")
	}

	// Return the new item HTML + updated progress bar + reset the form
	itemHTML := renderChecklistItemHTML(inspectionID, *item, nil)
	stats, _ := a.db.GetInspectionStats(ctx, inspectionID)
	progressHTML := renderProgressBarHTML(stats)

	return c.HTML(http.StatusOK, itemHTML+progressHTML)
}

// POST /api/inspections/:id/adhoc/:adhocId/status — update an ad-hoc item's status.
func (a *App) UpdateAdhocItemStatusHandler(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	adhocID, err := strconv.Atoi(c.Param("adhocId"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid adhoc item id")
	}

	statusStr := c.FormValue("status")

	if statusStr == "clear" {
		if err := a.db.UpdateAdhocItemStatus(ctx, inspectionID, adhocID, nil, nil); err != nil {
			log.Printf("clear adhoc status error: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to clear status")
		}
	} else {
		status := ItemStatus(statusStr)
		if !ValidItemStatuses[status] {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid status")
		}
		var notesPtr *string
		if n := strings.TrimSpace(c.FormValue("notes")); n != "" {
			notesPtr = &n
		}
		if err := a.db.UpdateAdhocItemStatus(ctx, inspectionID, adhocID, &status, notesPtr); err != nil {
			log.Printf("update adhoc status error: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to update status")
		}
	}

	// Return updated item + progress bar (include existing photos so gallery isn't reset)
	item, err := a.db.GetAdhocItem(ctx, inspectionID, adhocID)
	if err != nil {
		log.Printf("get adhoc item error: %v", err)
		stats, _ := a.db.GetInspectionStats(ctx, inspectionID)
		return c.HTML(http.StatusOK, renderProgressBarHTML(stats))
	}

	adhocPhotos, _ := a.db.GetInspectionPhotosByAdhocItem(ctx, inspectionID, adhocID)
	stats, _ := a.db.GetInspectionStats(ctx, inspectionID)
	return c.HTML(http.StatusOK, renderChecklistItemHTML(inspectionID, *item, adhocPhotos)+renderProgressBarHTML(stats))
}

// DELETE /api/inspections/:id/adhoc/:adhocId — remove an ad-hoc item.
func (a *App) DeleteAdhocItemHandler(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	adhocID, err := strconv.Atoi(c.Param("adhocId"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid adhoc item id")
	}

	if err := a.db.DeleteAdhocItem(ctx, inspectionID, adhocID); err != nil {
		log.Printf("delete adhoc item error: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to delete item")
	}

	// Return empty (HTMX will remove the element) + updated progress
	stats, _ := a.db.GetInspectionStats(ctx, inspectionID)
	return c.HTML(http.StatusOK, renderProgressBarHTML(stats))
}

// ─── HTML Rendering Helpers ─────────────────────────────────────

// renderProgressBarHTML generates the OOB-swappable progress bar fragment.
func renderProgressBarHTML(stats InspectionStats) string {
	return fmt.Sprintf(`
		<div id="inspection-progress" hx-swap-oob="true">
			<div class="flex items-center justify-between text-xs mb-1">
				<span class="font-semibold text-duke-dark">%d / %d items</span>
				<span class="text-gray-400">%d%%</span>
			</div>
			<div class="w-full bg-gray-200 rounded-full h-2">
				<div class="h-2 rounded-full bg-duke-teal transition-all duration-300" style="width: %d%%"></div>
			</div>
			<div class="flex gap-3 mt-1.5 text-[11px]">
				<span class="text-green-600">✓ %d pass</span>
				<span class="text-red-600">✗ %d fail</span>
				<span class="text-amber-600">⚠ %d attention</span>
			</div>
		</div>
	`, stats.Completed, stats.Total, stats.ProgressPct, stats.ProgressPct,
		stats.Passed, stats.Failed, stats.NeedsAttention)
}

// renderChecklistItemHTML produces the HTML for a single checklist item row.
// Works for both template items and ad-hoc items (uses different status endpoint).
// photos may be nil (e.g., when returning from status update — photos are lazy-loaded).
func renderChecklistItemHTML(inspectionID int, item InspectionChecklistItem, photos []InspectionPhoto) string {
	currentStatus := ""
	if item.Status != nil {
		currentStatus = string(*item.Status)
	}

	// Determine the correct endpoint based on item type
	var statusEndpoint string
	var itemDivID string
	if item.IsAdhoc {
		statusEndpoint = fmt.Sprintf("/api/inspections/%d/adhoc/%d/status", inspectionID, item.ItemID)
		itemDivID = fmt.Sprintf("adhoc-%d", item.ItemID)
	} else {
		statusEndpoint = fmt.Sprintf("/api/inspections/%d/item/%d/status", inspectionID, item.ItemID)
		itemDivID = fmt.Sprintf("item-%d", item.ItemID)
	}

	// Status button helper
	statusBtn := func(status, label, activeColor, activeBg, icon string) string {
		isActive := currentStatus == status
		var btnClass string
		if isActive {
			btnClass = fmt.Sprintf("px-3 py-2 rounded-lg text-xs font-bold transition-all %s %s ring-2 ring-offset-1", activeColor, activeBg)
		} else {
			btnClass = "px-3 py-2 rounded-lg text-xs font-medium transition-all text-gray-400 bg-gray-100 hover:bg-gray-200 active:scale-95"
		}
		postStatus := status
		if isActive {
			postStatus = "clear"
		}
		return fmt.Sprintf(
			`<button hx-post="%s" hx-vals='{"status":"%s"}' hx-target="#%s" hx-swap="outerHTML" class="%s">%s %s</button>`,
			statusEndpoint, postStatus, itemDivID, btnClass, icon, label,
		)
	}

	// Description line
	descHTML := ""
	if item.Description != nil && *item.Description != "" {
		descHTML = fmt.Sprintf(`<p class="text-xs text-gray-400 mt-0.5">%s</p>`, escapeHTML(*item.Description))
	}

	// Photo required indicator
	photoIndicatorHTML := ""
	if item.RequirePhoto {
		photoIndicatorHTML = `<span class="inline-flex items-center gap-0.5 text-[10px] text-gray-400 ml-1" title="Photo required"><svg class="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 9a2 2 0 012-2h.93a2 2 0 001.664-.89l.812-1.22A2 2 0 0110.07 4h3.86a2 2 0 011.664.89l.812 1.22A2 2 0 0018.07 7H19a2 2 0 012 2v9a2 2 0 01-2 2H5a2 2 0 01-2-2V9z"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 13a3 3 0 11-6 0 3 3 0 016 0z"/></svg></span>`
	}

	// Ad-hoc badge + delete button
	adhocHTML := ""
	if item.IsAdhoc {
		adhocHTML = fmt.Sprintf(`
			<div class="flex items-center gap-1.5 mt-1">
				<span class="inline-flex items-center text-[10px] text-indigo-600 bg-indigo-50 rounded-full px-2 py-0.5 font-medium">+ Ad-hoc</span>
				<button hx-delete="/api/inspections/%d/adhoc/%d" hx-target="#%s" hx-swap="outerHTML"
					hx-confirm="Remove this ad-hoc item?"
					class="text-[10px] text-gray-400 hover:text-red-500 transition-colors">remove</button>
			</div>`, inspectionID, item.ItemID, itemDivID)
	}

	// Notes from response
	notesHTML := ""
	if item.ResponseNotes != nil && *item.ResponseNotes != "" {
		notesHTML = fmt.Sprintf(`<div class="mt-1.5 text-xs text-gray-500 italic bg-gray-50 rounded px-2 py-1">%s</div>`, escapeHTML(*item.ResponseNotes))
	}

	// Photo gallery (HTMX-driven: thumbnails + upload + delete)
	photoGalleryHTML := ""
	if item.IsAdhoc {
		photoGalleryHTML = renderAdhocPhotoGalleryHTML(inspectionID, item.ItemID, photos)
	} else {
		photoGalleryHTML = renderPhotoGalleryHTML(inspectionID, item.ItemID, photos)
	}

	// Border color based on status
	borderClass := "border-gray-100"
	if currentStatus == "pass" {
		borderClass = "border-green-200 bg-green-50/30"
	} else if currentStatus == "fail" {
		borderClass = "border-red-200 bg-red-50/30"
	} else if currentStatus == "needs_attention" {
		borderClass = "border-amber-200 bg-amber-50/30"
	}

	return fmt.Sprintf(`
		<div id="%s" class="border rounded-xl p-3 %s transition-colors">
			<div class="flex items-start justify-between gap-2">
				<div class="flex-1 min-w-0">
					<span class="text-sm font-medium text-duke-dark">%s</span>%s
					%s
					%s
					%s
				</div>
			</div>
			%s
			<div class="flex gap-2 mt-2.5">
				%s
				%s
				%s
			</div>
		</div>`,
		itemDivID, borderClass,
		escapeHTML(item.Label), photoIndicatorHTML,
		descHTML,
		adhocHTML,
		notesHTML,
		photoGalleryHTML,
		statusBtn("pass", "Pass", "text-green-700", "bg-green-100 ring-green-400", "✓"),
		statusBtn("fail", "Fail", "text-red-700", "bg-red-100 ring-red-400", "✗"),
		statusBtn("needs_attention", "Attention", "text-amber-700", "bg-amber-100 ring-amber-400", "⚠"),
	)
}

