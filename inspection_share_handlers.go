package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

// POST /api/inspections/:id/share — generate (or return existing) share token + URL.
func (a *App) GenerateShareLink(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid id"})
	}

	token, err := a.db.GetOrCreateShareToken(ctx, id)
	if err != nil {
		log.Printf("generate share token error: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to generate link"})
	}

	// Build absolute URL — prefer X-Forwarded-Host for reverse-proxied deployments
	host := c.Request().Header.Get("X-Forwarded-Host")
	if host == "" {
		host = c.Request().Host
	}
	scheme := "https"
	if c.Request().TLS == nil && (host == "localhost" || strings.HasPrefix(host, "localhost:")) {
		scheme = "http"
	}
	url := fmt.Sprintf("%s://%s/share/inspection/%s", scheme, host, token)

	return c.JSON(http.StatusOK, map[string]string{
		"url":   url,
		"token": token,
	})
}

// GET /share/inspection/:token — public read-only inspection view (no auth required).
func (a *App) ShareInspectionPage(c echo.Context) error {
	ctx := c.Request().Context()

	token := c.Param("token")
	if token == "" {
		return echo.NewHTTPError(http.StatusNotFound, "Invalid link")
	}

	checklist, err := a.db.GetInspectionByShareToken(ctx, token)
	if err != nil {
		log.Printf("share inspection lookup error: %v", err)
		return echo.NewHTTPError(http.StatusNotFound, "Inspection not found or link is invalid")
	}

	photosByItem, err := a.db.GetAllInspectionPhotos(ctx, checklist.Inspection.ID)
	if err != nil {
		photosByItem = &PhotosByItemResult{
			ByItemID:      make(map[int][]InspectionPhoto),
			ByAdhocItemID: make(map[int][]InspectionPhoto),
		}
	}

	// Pre-render read-only item cards for each category
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
			sb.WriteString(renderShareItemHTML(item, photos))
		}
		rendered = append(rendered, RenderedCategory{
			Name:      cat.Name,
			ItemCount: len(cat.Items),
			ItemsHTML: sb.String(),
		})
	}

	return c.Render(http.StatusOK, "inspection_share", map[string]interface{}{
		"Inspection":         checklist.Inspection,
		"RenderedCategories": rendered,
		"Stats":              checklist.Stats,
	})
}

// GET /inspection/:id/print — standalone print-optimized HTML page.
func (a *App) PrintInspectionPage(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}

	checklist, err := a.db.GetInspectionChecklist(ctx, id)
	if err != nil {
		log.Printf("print inspection error: %v", err)
		return echo.NewHTTPError(http.StatusNotFound, "Inspection not found")
	}

	photosByItem, err := a.db.GetAllInspectionPhotos(ctx, id)
	if err != nil {
		photosByItem = &PhotosByItemResult{
			ByItemID:      make(map[int][]InspectionPhoto),
			ByAdhocItemID: make(map[int][]InspectionPhoto),
		}
	}

	host := c.Request().Header.Get("X-Forwarded-Host")
	if host == "" {
		host = c.Request().Host
	}

	c.Response().Header().Set("Content-Type", "text/html; charset=utf-8")
	return c.HTML(http.StatusOK, renderPrintHTML(checklist, photosByItem, host))
}
