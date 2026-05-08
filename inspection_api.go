package main

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
)

// ─── Inspection Admin Pages ──────────────────────────────────────

// GET /admin/inspections — list all inspection templates
func (a *App) InspectionTemplatesPage(c echo.Context) error {
	ctx := c.Request().Context()

	templates, err := a.db.ListInspectionTemplatesAdmin(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to load templates")
	}

	return c.Render(http.StatusOK, "admin_inspections", map[string]interface{}{
		"Templates": templates,
	})
}

// GET /admin/inspections/:id — view/edit a single template with inline categories and items
func (a *App) InspectionTemplateDetailPage(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid template id")
	}

	t, err := a.db.GetInspectionTemplateFull(ctx, id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "template not found")
	}

	return c.Render(http.StatusOK, "admin_inspection_detail", map[string]interface{}{
		"Template": t,
	})
}

// ─── Template CRUD (HTMX — returns HTML fragments) ──────────────

// POST /api/admin/inspection-template
func (a *App) CreateInspectionTemplate(c echo.Context) error {
	name := c.FormValue("name")
	description := c.FormValue("description")

	if name == "" {
		return c.String(http.StatusBadRequest, "Name is required")
	}

	var descPtr *string
	if description != "" {
		descPtr = &description
	}

	tmpl, err := a.db.CreateInspectionTemplate(c.Request().Context(), name, descPtr)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to create template: "+err.Error())
	}

	at := AdminInspectionTemplate{InspectionTemplate: *tmpl, CategoryCount: 0, ItemCount: 0}
	return renderFragment(c, "inspection-template-item", at)
}

// PUT /api/admin/inspection-template/:id
func (a *App) UpdateInspectionTemplate(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	name := c.FormValue("name")
	description := c.FormValue("description")

	if name == "" {
		return c.String(http.StatusBadRequest, "Name is required")
	}

	var descPtr *string
	if description != "" {
		descPtr = &description
	}

	tmpl, err := a.db.UpdateInspectionTemplate(c.Request().Context(), id, name, descPtr)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to update template")
	}

	return renderFragment(c, "inspection-template-header", tmpl)
}

// POST /api/admin/inspection-template/:id/toggle
func (a *App) ToggleInspectionTemplate(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	ctx := c.Request().Context()

	tmpl, err := a.db.ToggleInspectionTemplateActive(ctx, id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	// Check if request came from the list page or detail page
	// If hx-target contains "template-header", return header fragment
	target := c.Request().Header.Get("HX-Target")
	if target == "template-header" {
		return renderFragment(c, "inspection-template-header", tmpl)
	}

	// Return list item fragment with counts
	templates, _ := a.db.ListInspectionTemplatesAdmin(ctx)
	for _, t := range templates {
		if t.ID == tmpl.ID {
			return renderFragment(c, "inspection-template-item", t)
		}
	}

	at := AdminInspectionTemplate{InspectionTemplate: *tmpl}
	return renderFragment(c, "inspection-template-item", at)
}

// POST /api/admin/inspection-template/:id/duplicate
func (a *App) DuplicateInspectionTemplate(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	src, err := a.db.GetInspectionTemplateFull(ctx, id)
	if err != nil {
		return c.String(http.StatusNotFound, "Template not found")
	}

	copyName := src.Name + " (Copy)"
	newT, err := a.db.CreateInspectionTemplate(ctx, copyName, src.Description)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to duplicate template")
	}

	for _, cat := range src.Categories {
		newCat, err := a.db.CreateInspectionCategory(ctx, newT.ID, cat.Name, cat.SortOrder)
		if err != nil {
			continue
		}
		for _, item := range cat.Items {
			a.db.CreateInspectionItem(ctx, newCat.ID, item.Label, item.Description, item.RequirePhoto, item.SortOrder)
		}
	}

	// Re-render the full template list
	templates, err := a.db.ListInspectionTemplatesAdmin(ctx)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to reload templates")
	}

	return renderFragment(c, "inspection-template-list", map[string]interface{}{
		"Templates": templates,
	})
}

// DELETE /api/admin/inspection-template/:id
func (a *App) DeleteInspectionTemplate(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	if err := a.db.DeleteInspectionTemplate(c.Request().Context(), id); err != nil {
		return c.String(http.StatusInternalServerError, "Failed to delete template")
	}

	// Return empty to remove element from DOM
	return c.HTML(http.StatusOK, "")
}

// ─── Category CRUD (HTMX) ───────────────────────────────────────

// POST /api/admin/inspection-template/:id/category
func (a *App) CreateTemplateCategory(c echo.Context) error {
	templateID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	name := c.FormValue("name")
	if name == "" {
		return c.String(http.StatusBadRequest, "Category name is required")
	}

	cat, err := a.db.CreateInspectionCategory(c.Request().Context(), templateID, name, 0)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to create category: "+err.Error())
	}

	return renderFragment(c, "inspection-category-item", cat)
}

// PUT /api/admin/inspection-category/:id
func (a *App) UpdateTemplateCategory(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	name := c.FormValue("name")
	if name == "" {
		return c.String(http.StatusBadRequest, "Category name is required")
	}

	// Get current sort order to preserve it
	currentCat, err := a.db.GetCategoryWithItems(c.Request().Context(), id)
	if err != nil {
		return c.String(http.StatusNotFound, "Category not found")
	}

	_, err = a.db.UpdateInspectionCategory(c.Request().Context(), id, name, currentCat.SortOrder)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to update category")
	}

	// Re-fetch with items
	fullCat, err := a.db.GetCategoryWithItems(c.Request().Context(), id)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to reload category")
	}

	return renderFragment(c, "inspection-category-item", fullCat)
}

// DELETE /api/admin/inspection-category/:id
func (a *App) DeleteTemplateCategory(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	if err := a.db.DeleteInspectionCategory(c.Request().Context(), id); err != nil {
		return c.String(http.StatusInternalServerError, "Failed to delete category")
	}

	return c.HTML(http.StatusOK, "")
}

// ─── Item CRUD (HTMX) ───────────────────────────────────────────

// POST /api/admin/inspection-category/:id/item
func (a *App) CreateTemplateItem(c echo.Context) error {
	categoryID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	label := c.FormValue("label")
	if label == "" {
		return c.String(http.StatusBadRequest, "Item label is required")
	}

	descriptionVal := c.FormValue("description")
	var descPtr *string
	if descriptionVal != "" {
		descPtr = &descriptionVal
	}

	requirePhoto := c.FormValue("require_photo") == "on" || c.FormValue("require_photo") == "true"

	item, err := a.db.CreateInspectionItem(c.Request().Context(), categoryID, label, descPtr, requirePhoto, 0)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to create item: "+err.Error())
	}

	return renderFragment(c, "inspection-item-row", item)
}

// PUT /api/admin/inspection-item/:id
func (a *App) UpdateTemplateItem(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	label := c.FormValue("label")
	if label == "" {
		return c.String(http.StatusBadRequest, "Item label is required")
	}

	descriptionVal := c.FormValue("description")
	var descPtr *string
	if descriptionVal != "" {
		descPtr = &descriptionVal
	}

	requirePhoto := c.FormValue("require_photo") == "on" || c.FormValue("require_photo") == "true"

	// Preserve existing sort order
	sortOrderStr := c.FormValue("sort_order")
	sortOrder := 0
	if sortOrderStr != "" {
		sortOrder, _ = strconv.Atoi(sortOrderStr)
	}

	item, err := a.db.UpdateInspectionItem(c.Request().Context(), id, label, descPtr, requirePhoto, sortOrder)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to update item")
	}

	return renderFragment(c, "inspection-item-row", item)
}

// DELETE /api/admin/inspection-item/:id
func (a *App) DeleteTemplateItem(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	if err := a.db.DeleteInspectionItem(c.Request().Context(), id); err != nil {
		return c.String(http.StatusInternalServerError, "Failed to delete item")
	}

	return c.HTML(http.StatusOK, "")
}
