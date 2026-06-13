package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

// ─── PropertyOS Ingest (claudecode-u61f) ────────────────────────
// Inbound, server-to-server upload path used by PropertyOS's
// /api/buildings/[id]/photos proxy. The PropertyOS proxy has already
// authenticated the property manager and verified building access; this route
// trusts it via a shared bearer token (DUKECAM_INGEST_TOKEN) and records the
// PM as the uploader. The no-login worker path (/api/upload) is unaffected.

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// slugify turns a building name into a URL-safe project slug, e.g.
// "Stockbridge Michigan, LLC" → "stockbridge-michigan-llc".
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugNonAlnum.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// ingestAuth gates the PropertyOS ingest routes with the shared bearer token.
// Fails closed: a route is unreachable until DUKECAM_INGEST_TOKEN is configured.
func (a *App) ingestAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		expected := a.config.IngestToken
		if expected == "" {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "ingest not configured"})
		}
		got := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
		if got == "" || got != expected {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		}
		return next(c)
	}
}

func optionalIntForm(c echo.Context, field string) *int {
	if v := c.FormValue(field); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return &n
		}
	}
	return nil
}

func optionalStrForm(c echo.Context, field string) *string {
	if v := strings.TrimSpace(c.FormValue(field)); v != "" {
		return &v
	}
	return nil
}

// PropertyOSIngestPhoto handles POST /api/propertyos/photos.
// Multipart form: file, building_id (req), building_name, building_address,
// dukecam_slug, scope, unit_id, tenant_id, tenant_name, caption, tag,
// uploader_name. Resolves-or-creates the building's project, matches the
// uploader to a DukeCam worker by name, saves the photo, and returns the slug
// so PropertyOS can persist buildings.dukecam_slug.
func (a *App) PropertyOSIngestPhoto(c echo.Context) error {
	ctx := c.Request().Context()

	buildingID, err := strconv.Atoi(c.FormValue("building_id"))
	if err != nil || buildingID <= 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid building_id"})
	}
	buildingName := strings.TrimSpace(c.FormValue("building_name"))
	if buildingName == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "building_name required"})
	}

	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "no file uploaded"})
	}

	// Resolve (or create) the project that backs this building.
	project, err := a.db.GetOrCreateProjectForBuilding(ctx, c.FormValue("dukecam_slug"), buildingName, optionalStrForm(c, "building_address"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project resolve failed"})
	}

	// Scope: explicit value wins; otherwise infer from the presence of a
	// unit/tenant target.
	unitID := optionalIntForm(c, "unit_id")
	tenantID := optionalIntForm(c, "tenant_id")
	scope := strings.ToLower(strings.TrimSpace(c.FormValue("scope")))
	if scope != "property" && scope != "tenant" {
		if unitID != nil || tenantID != nil {
			scope = "tenant"
		} else {
			scope = "property"
		}
	}

	// Uploader: match the PM to an existing DukeCam worker by name; otherwise
	// stamp the free-text name so attribution survives even without a worker row.
	uploader := strings.TrimSpace(c.FormValue("uploader_name"))
	var workerID *int
	var workerName string
	if uploader != "" {
		if w, err := a.db.FindWorkerByName(ctx, uploader); err == nil {
			workerID = &w.ID
		} else {
			workerName = uploader
		}
	}

	photo, status, errMsg := a.storeUploadedPhoto(c, project, file, photoMeta{
		WorkerID:   workerID,
		WorkerName: workerName,
		Caption:    c.FormValue("caption"),
		Tag:        c.FormValue("tag"),
		BuildingID: &buildingID,
		UnitID:     unitID,
		TenantID:   tenantID,
		TenantName: optionalStrForm(c, "tenant_name"),
		Scope:      &scope,
	})
	if status != 0 {
		return c.JSON(status, map[string]string{"error": errMsg})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"status":     "ok",
		"photo_id":   photo.ID,
		"project_id": project.ID,
		"slug":       project.Slug,
		"filename":   photo.Filename,
	})
}

// PropertyOSBuildingPhotos handles GET /api/propertyos/photos?building_id=&unit_id=&tenant_id=&limit=
// Returns the building's photos (optionally scoped) for the PropertyOS grid.
func (a *App) PropertyOSBuildingPhotos(c echo.Context) error {
	ctx := c.Request().Context()

	buildingID, err := strconv.Atoi(c.QueryParam("building_id"))
	if err != nil || buildingID <= 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid building_id"})
	}
	var unitID, tenantID *int
	if v := c.QueryParam("unit_id"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			unitID = &n
		}
	}
	if v := c.QueryParam("tenant_id"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tenantID = &n
		}
	}
	limit, _ := strconv.Atoi(c.QueryParam("limit"))

	photos, err := a.db.GetPhotosForBuilding(ctx, buildingID, unitID, tenantID, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "query failed"})
	}

	out := make([]map[string]interface{}, 0, len(photos))
	for _, p := range photos {
		item := map[string]interface{}{
			"id":          p.ID,
			"photo_url":   "/media/photo/" + p.ProjectSlug + "/" + p.Filename,
			"thumb_url":   "/media/thumb/" + p.ProjectSlug + "/" + p.Filename,
			"uploaded_at": p.UploadedAt,
			"uploader":    p.DisplayName(),
			"scope":       p.Scope,
			"unit_id":     p.UnitID,
			"tenant_id":   p.TenantID,
			"tenant_name": p.TenantName,
			"caption":     p.Caption,
			"tag":         p.Tag,
			"taken_at":    p.TakenAt,
		}
		out = append(out, item)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"building_id": buildingID,
		"count":       len(out),
		"photos":      out,
	})
}
