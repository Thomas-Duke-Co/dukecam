package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// ─── Upload Inspection Item Photo ────────────────────────────────

// POST /api/inspections/:id/items/:itemId/photos — upload a photo for a specific checklist item.
// Accepts multipart/form-data with a "file" field and optional "caption" field.
// Returns JSON with the new photo record on success.
func (a *App) UploadInspectionItemPhoto(c echo.Context) error {
	ctx := c.Request().Context()

	// Parse inspection ID
	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid inspection id"})
	}

	// Parse item ID from URL path
	itemID, err := strconv.Atoi(c.Param("itemId"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid item id"})
	}

	// Validate inspection exists and is not completed
	insp, err := a.db.GetInspectionByID(ctx, inspectionID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "inspection not found"})
	}
	if insp.Status == "completed" {
		return c.JSON(http.StatusConflict, map[string]string{"error": "cannot add photos to a completed inspection"})
	}

	// Read uploaded file
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "no file uploaded"})
	}

	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "cannot open file"})
	}
	defer src.Close()

	data, err := io.ReadAll(src)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "cannot read file"})
	}

	// Validate file size
	maxBytes := a.config.MaxUploadMB * 1024 * 1024
	if len(data) > maxBytes {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("file too large (max %dMB)", a.config.MaxUploadMB),
		})
	}
	if len(data) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "empty file"})
	}

	// Validate image extension
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext == "" || !isAllowedExt(ext) {
		ext = ".jpg"
	}

	log.Printf("inspection photo upload: inspection=%d item=%d file=%s size=%d",
		inspectionID, itemID, file.Filename, len(data))

	// Process image (decode, orient, thumbnail, EXIF)
	processed, err := ProcessUpload(data)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid image file"})
	}

	// Generate unique filename and date-based directory
	uniqueName := uuid.New().String()[:32] + ext
	dateDir := time.Now().Format("2006/01/02")
	slug := "inspections"

	photoPath := filepath.Join(a.config.StoragePath, slug, dateDir, uniqueName)
	thumbPath := filepath.Join(a.config.ThumbPath, slug, dateDir, uniqueName)

	if processed.Processed {
		quality := 95
		if ext == ".png" {
			quality = 0 // lossless for PNG
		}
		if err := SaveImage(processed.Image, photoPath, quality); err != nil {
			log.Printf("save inspection photo error: %v", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "save failed"})
		}

		thumbQuality := 80
		if ext == ".png" {
			thumbQuality = 0
		}
		if err := SaveImage(processed.Thumb, thumbPath, thumbQuality); err != nil {
			log.Printf("save inspection thumb error: %v", err)
			thumbPath = "" // Non-fatal
		}
	} else {
		// Can't process (e.g., HEIC) — save raw bytes
		if err := SaveRaw(data, photoPath); err != nil {
			log.Printf("save raw inspection photo error: %v", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "save failed"})
		}
		thumbPath = ""
	}

	// Optional caption
	caption := strings.TrimSpace(c.FormValue("caption"))
	var captionPtr *string
	if caption != "" {
		captionPtr = &caption
	}

	// Optional thumb path
	var thumbPathPtr *string
	if thumbPath != "" {
		thumbPathPtr = &thumbPath
	}

	// Dimensions
	fileSize := len(data)
	var width, height *int
	if processed.Processed {
		w, h := processed.Width, processed.Height
		width, height = &w, &h
	}

	// Build photo record using the canonical model from inspection_repository.go
	photo := &InspectionPhoto{
		InspectionID: inspectionID,
		ItemID:       &itemID,
		Filename:     uniqueName,
		StoragePath:  photoPath,
		ThumbPath:    thumbPathPtr,
		Caption:      captionPtr,
		FileSize:     &fileSize,
		Width:        width,
		Height:       height,
		Lat:          processed.EXIF.Lat,
		Lng:          processed.EXIF.Lng,
	}

	// Insert into database
	if err := a.db.InsertInspectionPhoto(ctx, photo); err != nil {
		log.Printf("insert inspection photo error: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "database error"})
	}

	log.Printf("inspection photo uploaded: id=%d inspection=%d item=%d file=%s",
		photo.ID, inspectionID, itemID, uniqueName)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"id":            photo.ID,
		"inspection_id": inspectionID,
		"item_id":       itemID,
		"filename":      uniqueName,
		"thumb_url":     fmt.Sprintf("/api/inspections/photos/%d/thumb", photo.ID),
		"full_url":      fmt.Sprintf("/api/inspections/photos/%d", photo.ID),
		"file_size":     fileSize,
		"width":         width,
		"height":        height,
		"status":        "ok",
	})
}

// ─── Upload General Inspection Photo ─────────────────────────────

// POST /api/inspections/:id/photos — upload a photo attached to an inspection (optionally linked to an item).
// Form fields: file (required), item_id (optional), adhoc_item_id (optional), caption (optional).
// Returns JSON with photo ID and thumbnail URL.
func (a *App) UploadInspectionPhoto(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid inspection id"})
	}

	// Validate inspection exists
	if _, err := a.db.GetInspectionByID(ctx, inspectionID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "inspection not found"})
	}

	// Optional item linkage
	var itemID *int
	if idStr := c.FormValue("item_id"); idStr != "" {
		if id, err := strconv.Atoi(idStr); err == nil && id > 0 {
			itemID = &id
		}
	}

	var adhocItemID *int
	if idStr := c.FormValue("adhoc_item_id"); idStr != "" {
		if id, err := strconv.Atoi(idStr); err == nil && id > 0 {
			adhocItemID = &id
		}
	}

	caption := c.FormValue("caption")

	// Read uploaded file
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "no file uploaded"})
	}

	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "cannot open file"})
	}
	defer src.Close()

	data, err := io.ReadAll(src)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "cannot read file"})
	}

	maxBytes := a.config.MaxUploadMB * 1024 * 1024
	if len(data) > maxBytes {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("file too large (max %dMB)", a.config.MaxUploadMB),
		})
	}
	if len(data) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "empty file"})
	}

	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext == "" || !isAllowedExt(ext) {
		ext = ".jpg"
	}

	processed, err := ProcessUpload(data)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid image"})
	}

	uniqueName := uuid.New().String()[:32] + ext
	dateDir := time.Now().Format("2006/01/02")
	slug := "inspections"

	photoPath := filepath.Join(a.config.StoragePath, slug, dateDir, uniqueName)
	thumbPath := filepath.Join(a.config.ThumbPath, slug, dateDir, uniqueName)

	if processed.Processed {
		quality := 95
		if ext == ".png" {
			quality = 0
		}
		if err := SaveImage(processed.Image, photoPath, quality); err != nil {
			log.Printf("save inspection photo error: %v", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "save failed"})
		}

		thumbQuality := 80
		if ext == ".png" {
			thumbQuality = 0
		}
		if err := SaveImage(processed.Thumb, thumbPath, thumbQuality); err != nil {
			log.Printf("save inspection thumb error: %v", err)
			thumbPath = ""
		}
	} else {
		if err := SaveRaw(data, photoPath); err != nil {
			log.Printf("save raw inspection photo error: %v", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "save failed"})
		}
		thumbPath = ""
	}

	var captionPtr, thumbPathPtr *string
	if caption != "" {
		captionPtr = &caption
	}
	if thumbPath != "" {
		thumbPathPtr = &thumbPath
	}

	fileSize := len(data)
	var width, height *int
	if processed.Processed {
		w, h := processed.Width, processed.Height
		width, height = &w, &h
	}

	photo := &InspectionPhoto{
		InspectionID: inspectionID,
		ItemID:       itemID,
		AdhocItemID:  adhocItemID,
		Filename:     uniqueName,
		StoragePath:  photoPath,
		ThumbPath:    thumbPathPtr,
		Caption:      captionPtr,
		FileSize:     &fileSize,
		Width:        width,
		Height:       height,
		Lat:          processed.EXIF.Lat,
		Lng:          processed.EXIF.Lng,
	}

	if err := a.db.InsertInspectionPhoto(ctx, photo); err != nil {
		log.Printf("insert inspection photo error: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "database error"})
	}

	log.Printf("inspection photo uploaded: id=%d inspection=%d item=%v", photo.ID, inspectionID, itemID)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"id":        photo.ID,
		"filename":  uniqueName,
		"thumb_url": fmt.Sprintf("/api/inspections/photos/%d/thumb", photo.ID),
		"full_url":  fmt.Sprintf("/api/inspections/photos/%d", photo.ID),
		"status":    "ok",
	})
}

// ─── Serve Inspection Photo (full size) ──────────────────────────

// GET /api/inspections/photos/:id — serve the full-size inspection photo.
func (a *App) ServeInspectionPhoto(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid photo id")
	}

	photo, err := a.db.GetInspectionPhotoByID(c.Request().Context(), id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "photo not found")
	}

	return c.File(photo.StoragePath)
}

// ─── Serve Inspection Photo Thumbnail ────────────────────────────

// GET /api/inspections/photos/:id/thumb — serve the thumbnail (falls back to full-size).
func (a *App) ServeInspectionPhotoThumb(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid photo id")
	}

	photo, err := a.db.GetInspectionPhotoByID(c.Request().Context(), id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "photo not found")
	}

	if photo.ThumbPath != nil && *photo.ThumbPath != "" {
		return c.File(*photo.ThumbPath)
	}
	return c.File(photo.StoragePath)
}

// ─── Delete Inspection Photo ─────────────────────────────────────

// DELETE /api/inspections/:id/photos/:photoId — delete a photo from an inspection.
func (a *App) DeleteInspectionPhotoHandler(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid inspection id"})
	}

	photoID, err := strconv.Atoi(c.Param("photoId"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid photo id"})
	}

	if err := a.db.DeleteInspectionPhoto(ctx, inspectionID, photoID); err != nil {
		log.Printf("delete inspection photo error: %v", err)
		return c.JSON(http.StatusNotFound, map[string]string{"error": "photo not found"})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// ─── Get Photos for Item (JSON) ──────────────────────────────────

// GET /api/inspections/:id/items/:itemId/photos — list photos for a checklist item.
func (a *App) GetInspectionItemPhotos(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid inspection id"})
	}

	itemID, err := strconv.Atoi(c.Param("itemId"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid item id"})
	}

	photos, err := a.db.GetInspectionPhotosByItem(ctx, inspectionID, itemID)
	if err != nil {
		log.Printf("get inspection item photos error: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to load photos"})
	}

	type photoResponse struct {
		ID       int     `json:"id"`
		Filename string  `json:"filename"`
		Caption  *string `json:"caption,omitempty"`
		ThumbURL string  `json:"thumb_url"`
		FullURL  string  `json:"full_url"`
		FileSize *int    `json:"file_size,omitempty"`
		Width    *int    `json:"width,omitempty"`
		Height   *int    `json:"height,omitempty"`
	}

	result := make([]photoResponse, 0, len(photos))
	for _, p := range photos {
		result = append(result, photoResponse{
			ID:       p.ID,
			Filename: p.Filename,
			Caption:  p.Caption,
			ThumbURL: fmt.Sprintf("/api/inspections/photos/%d/thumb", p.ID),
			FullURL:  fmt.Sprintf("/api/inspections/photos/%d", p.ID),
			FileSize: p.FileSize,
			Width:    p.Width,
			Height:   p.Height,
		})
	}

	return c.JSON(http.StatusOK, result)
}

// ─── Get Photos for Item (HTMX fragment) ─────────────────────────

// GET /api/inspections/:id/item/:itemId/photos — returns the complete HTMX photo gallery for a checklist item.
func (a *App) GetItemPhotosHTML(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	itemID, err := strconv.Atoi(c.Param("itemId"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid item id")
	}

	photos, err := a.db.GetInspectionPhotosByItem(ctx, inspectionID, itemID)
	if err != nil {
		log.Printf("get item photos error: %v", err)
		return c.HTML(http.StatusOK, renderPhotoGalleryHTML(inspectionID, itemID, nil))
	}

	return c.HTML(http.StatusOK, renderPhotoGalleryHTML(inspectionID, itemID, photos))
}

// ─── Photo Rendering Helpers ─────────────────────────────────────

// renderItemPhotosHTML produces a horizontal scrollable strip of photo thumbnails.
func renderItemPhotosHTML(photos []InspectionPhoto) string {
	if len(photos) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(`<div class="flex gap-2 mt-2 overflow-x-auto pb-1 -mx-1 px-1">`)
	for _, p := range photos {
		sb.WriteString(fmt.Sprintf(
			`<button type="button" onclick="openLightbox(%d)" class="flex-shrink-0 rounded-lg overflow-hidden border border-gray-200 hover:border-duke-teal transition-colors focus:outline-none focus:ring-2 focus:ring-duke-teal/50">
				<img src="/api/inspections/photos/%d/thumb" alt="%s" loading="lazy" class="w-16 h-16 object-cover" />
			</button>`,
			p.ID, p.ID, escapeHTML(stringOrEmpty(p.Caption)),
		))
	}
	sb.WriteString(`</div>`)
	return sb.String()
}

// renderPhotoUploadBtnHTML produces the camera button for attaching photos to a checklist item.
func renderPhotoUploadBtnHTML(inspectionID, itemID int, isAdhoc bool) string {
	adhocStr := "false"
	if isAdhoc {
		adhocStr = "true"
	}
	return fmt.Sprintf(
		`<button type="button" onclick="openPhotoCapture(%d, %d, %s)"
			class="flex items-center gap-1 px-2 py-1.5 text-[11px] text-gray-400 hover:text-duke-teal bg-gray-50 hover:bg-gray-100 rounded-lg transition-colors active:scale-95"
			title="Attach photo">
			<svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
				<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 9a2 2 0 012-2h.93a2 2 0 001.664-.89l.812-1.22A2 2 0 0110.07 4h3.86a2 2 0 011.664.89l.812 1.22A2 2 0 0018.07 7H19a2 2 0 012 2v9a2 2 0 01-2 2H5a2 2 0 01-2-2V9z"/>
				<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 13a3 3 0 11-6 0 3 3 0 016 0z"/>
			</svg>
			<span>Photo</span>
		</button>`,
		inspectionID, itemID, adhocStr,
	)
}

// stringOrEmpty returns the dereferenced string or empty.
func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ─── HTMX Photo Handlers (return HTML gallery fragments) ─────────

// POST /api/inspections/:id/item/:itemId/photo — HTMX multipart upload.
// Uses "photo" file field (matches the HTMX file input name="photo" with capture="environment").
// Returns the complete photo gallery HTML fragment for this item.
func (a *App) UploadInspectionItemPhotoHTMX(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	itemID, err := strconv.Atoi(c.Param("itemId"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid item id")
	}

	// Read uploaded file — field name is "photo" (from the HTMX file input)
	file, err := c.FormFile("photo")
	if err != nil {
		// No file selected — return existing gallery unchanged
		photos, _ := a.db.GetInspectionPhotosByItem(ctx, inspectionID, itemID)
		return c.HTML(http.StatusOK, renderPhotoGalleryHTML(inspectionID, itemID, photos))
	}

	src, err := file.Open()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "cannot open file")
	}
	defer src.Close()

	data, err := io.ReadAll(src)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "cannot read file")
	}

	maxBytes := a.config.MaxUploadMB * 1024 * 1024
	if len(data) > maxBytes || len(data) == 0 {
		photos, _ := a.db.GetInspectionPhotosByItem(ctx, inspectionID, itemID)
		return c.HTML(http.StatusOK, renderPhotoGalleryHTML(inspectionID, itemID, photos))
	}

	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext == "" || !isAllowedExt(ext) {
		ext = ".jpg"
	}

	processed, err := ProcessUpload(data)
	if err != nil {
		photos, _ := a.db.GetInspectionPhotosByItem(ctx, inspectionID, itemID)
		return c.HTML(http.StatusOK, renderPhotoGalleryHTML(inspectionID, itemID, photos))
	}

	uniqueName := uuid.New().String()[:32] + ext
	dateDir := time.Now().Format("2006/01/02")

	photoPath := filepath.Join(a.config.StoragePath, "inspections", dateDir, uniqueName)
	thumbPath := filepath.Join(a.config.ThumbPath, "inspections", dateDir, uniqueName)

	if processed.Processed {
		quality := 95
		if ext == ".png" {
			quality = 0
		}
		if err := SaveImage(processed.Image, photoPath, quality); err != nil {
			log.Printf("save inspection photo error: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "save failed")
		}
		thumbQuality := 80
		if ext == ".png" {
			thumbQuality = 0
		}
		if err := SaveImage(processed.Thumb, thumbPath, thumbQuality); err != nil {
			log.Printf("save inspection thumb error: %v", err)
			thumbPath = ""
		}
	} else {
		if err := SaveRaw(data, photoPath); err != nil {
			log.Printf("save raw inspection photo error: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "save failed")
		}
		thumbPath = ""
	}

	var thumbPathPtr *string
	if thumbPath != "" {
		thumbPathPtr = &thumbPath
	}
	fileSize := len(data)
	var width, height *int
	if processed.Processed {
		w, h := processed.Width, processed.Height
		width, height = &w, &h
	}

	photo := &InspectionPhoto{
		InspectionID: inspectionID,
		ItemID:       &itemID,
		Filename:     uniqueName,
		StoragePath:  photoPath,
		ThumbPath:    thumbPathPtr,
		FileSize:     &fileSize,
		Width:        width,
		Height:       height,
		Lat:          processed.EXIF.Lat,
		Lng:          processed.EXIF.Lng,
	}

	if err := a.db.InsertInspectionPhoto(ctx, photo); err != nil {
		log.Printf("insert inspection photo error: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "database error")
	}

	log.Printf("inspection photo uploaded (HTMX): id=%d inspection=%d item=%d", photo.ID, inspectionID, itemID)

	// Return complete updated gallery for this item
	photos, _ := a.db.GetInspectionPhotosByItem(ctx, inspectionID, itemID)
	return c.HTML(http.StatusOK, renderPhotoGalleryHTML(inspectionID, itemID, photos))
}

// DELETE /api/inspections/:id/photo/:photoId — HTMX delete with hx-confirm.
// Returns the updated photo gallery HTML for the item the photo belonged to.
func (a *App) DeleteInspectionItemPhotoHTMX(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	photoID, err := strconv.Atoi(c.Param("photoId"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid photo id")
	}

	// Get photo info before deleting (need item ID + file paths)
	photo, err := a.db.GetInspectionPhotoByID(ctx, photoID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "photo not found")
	}

	// Delete from DB
	if err := a.db.DeleteInspectionPhoto(ctx, inspectionID, photoID); err != nil {
		log.Printf("delete inspection photo error: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to delete photo")
	}

	// Clean up files (best-effort)
	if err := os.Remove(photo.StoragePath); err != nil && !os.IsNotExist(err) {
		log.Printf("remove inspection photo file warning: %v", err)
	}
	if photo.ThumbPath != nil && *photo.ThumbPath != "" {
		if err := os.Remove(*photo.ThumbPath); err != nil && !os.IsNotExist(err) {
			log.Printf("remove inspection thumb warning: %v", err)
		}
	}

	log.Printf("inspection photo deleted (HTMX): id=%d inspection=%d", photoID, inspectionID)

	// Return updated gallery for the item (template or ad-hoc)
	if photo.ItemID != nil && *photo.ItemID > 0 {
		photos, _ := a.db.GetInspectionPhotosByItem(ctx, inspectionID, *photo.ItemID)
		return c.HTML(http.StatusOK, renderPhotoGalleryHTML(inspectionID, *photo.ItemID, photos))
	}
	if photo.AdhocItemID != nil && *photo.AdhocItemID > 0 {
		photos, _ := a.db.GetInspectionPhotosByAdhocItem(ctx, inspectionID, *photo.AdhocItemID)
		return c.HTML(http.StatusOK, renderAdhocPhotoGalleryHTML(inspectionID, *photo.AdhocItemID, photos))
	}
	return c.HTML(http.StatusOK, "")
}

// ─── HTMX Photo Gallery Renderer ─────────────────────────────────

// renderPhotoGalleryHTML generates the complete HTMX-driven photo section for a checklist item.
// Includes:
//   - Thumbnail previews with links to full-size images (opens in new tab)
//   - Delete button per photo with hx-confirm prompt
//   - Hidden file input with capture="environment" (triggers mobile camera)
//   - HTMX upload trigger on file change
//   - Loading spinner during upload
//   - Photo count badge
//
// The entire gallery is wrapped in a single div with id="photos-{itemId}" so that
// HTMX upload/delete responses can swap it atomically.
func renderPhotoGalleryHTML(inspectionID, itemID int, photos []InspectionPhoto) string {
	galleryID := fmt.Sprintf("photos-%d", itemID)
	inputID := fmt.Sprintf("photo-input-%d", itemID)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`<div id="%s" class="mt-2">`, galleryID))

	// ── Thumbnail strip with per-photo delete buttons ──
	if len(photos) > 0 {
		sb.WriteString(`<div class="flex flex-wrap gap-2 mb-2">`)
		for _, p := range photos {
			thumbURL := fmt.Sprintf("/api/inspections/photos/%d/thumb", p.ID)

			sb.WriteString(fmt.Sprintf(`
				<div class="relative group">
					<button type="button" onclick="openLightbox(%d, '%s')" data-photo-id="%d" class="block focus:outline-none focus:ring-2 focus:ring-duke-teal/50 rounded-lg">
						<img src="%s" alt="Inspection photo"
							class="w-16 h-16 rounded-lg object-cover border border-gray-200 shadow-sm hover:shadow-md transition-shadow"
							loading="lazy"/>
					</button>
					<button type="button"
						hx-delete="/api/inspections/%d/photo/%d"
						hx-target="#%s"
						hx-swap="outerHTML"
						hx-confirm="Delete this photo?"
						class="absolute -top-1.5 -right-1.5 w-5 h-5 bg-red-500 hover:bg-red-600 text-white rounded-full text-[10px] font-bold leading-none flex items-center justify-center shadow opacity-0 group-hover:opacity-100 active:opacity-100 focus:opacity-100 transition-opacity active:scale-90"
						title="Delete photo">&times;</button>
				</div>`,
				p.ID, galleryID, p.ID, thumbURL,
				inspectionID, p.ID,
				galleryID,
			))
		}
		sb.WriteString(`</div>`)
	}

	// ── Upload button (label triggers hidden file input) + rapid shoot + spinner ──
	sb.WriteString(fmt.Sprintf(`
		<div class="flex items-center gap-2">
			<label for="%s"
				class="inline-flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium text-gray-500 bg-gray-50 hover:bg-gray-100 border border-gray-200 border-dashed rounded-lg cursor-pointer transition-colors active:scale-95">
				<svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
					<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 9a2 2 0 012-2h.93a2 2 0 001.664-.89l.812-1.22A2 2 0 0110.07 4h3.86a2 2 0 011.664.89l.812 1.22A2 2 0 0018.07 7H19a2 2 0 012 2v9a2 2 0 01-2 2H5a2 2 0 01-2-2V9z"/>
					<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 13a3 3 0 11-6 0 3 3 0 016 0z"/>
				</svg>
				<span>Add Photo</span>
			</label>
			<button type="button" onclick="startRapidShoot(%d, %d, false)"
				class="inline-flex items-center gap-1 px-2.5 py-1.5 text-xs font-medium text-duke-teal bg-duke-teal/5 hover:bg-duke-teal/10 border border-duke-teal/20 rounded-lg cursor-pointer transition-colors active:scale-95"
				title="Rapid Shoot: keep camera open between shots">
				<svg class="w-3.5 h-3.5" fill="currentColor" viewBox="0 0 20 20">
					<path d="M11.983 1.907a.75.75 0 00-1.292-.657l-8.5 9.5A.75.75 0 002.75 12h6.572l-1.305 6.093a.75.75 0 001.292.657l8.5-9.5A.75.75 0 0017.25 8h-6.572l1.305-6.093z"/>
				</svg>
				Rapid
			</button>
			<input type="file" id="%s" name="photo" accept="image/*" capture="environment"
				class="hidden"
				hx-post="/api/inspections/%d/item/%d/photo"
				hx-target="#%s"
				hx-swap="outerHTML"
				hx-encoding="multipart/form-data"
				hx-trigger="change"
				hx-indicator="#upload-spinner-%d"/>
			<div id="upload-spinner-%d" class="htmx-indicator">
				<svg class="animate-spin w-4 h-4 text-duke-teal" fill="none" viewBox="0 0 24 24">
					<circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
					<path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
				</svg>
			</div>`,
		inputID,
		inspectionID, itemID,
		inputID,
		inspectionID, itemID, galleryID,
		itemID, itemID,
	))

	// Photo count badge
	if len(photos) > 0 {
		sb.WriteString(fmt.Sprintf(
			`<span class="text-[10px] text-gray-400">%d photo%s</span>`,
			len(photos), pluralS(len(photos)),
		))
	}

	sb.WriteString(`</div>`)  // close upload row
	sb.WriteString(`</div>`)  // close gallery container

	return sb.String()
}

// renderAdhocPhotoGalleryHTML generates the HTMX photo gallery for an ad-hoc checklist item.
// Same visual treatment as template item galleries but uses adhoc-specific endpoints and IDs.
func renderAdhocPhotoGalleryHTML(inspectionID, adhocItemID int, photos []InspectionPhoto) string {
	galleryID := fmt.Sprintf("photos-adhoc-%d", adhocItemID)
	inputID := fmt.Sprintf("photo-input-adhoc-%d", adhocItemID)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`<div id="%s" class="mt-2">`, galleryID))

	// ── Thumbnail strip with per-photo delete buttons ──
	if len(photos) > 0 {
		sb.WriteString(`<div class="flex flex-wrap gap-2 mb-2">`)
		for _, p := range photos {
			thumbURL := fmt.Sprintf("/api/inspections/photos/%d/thumb", p.ID)

			sb.WriteString(fmt.Sprintf(`
				<div class="relative group">
					<button type="button" onclick="openLightbox(%d, '%s')" data-photo-id="%d" class="block focus:outline-none focus:ring-2 focus:ring-duke-teal/50 rounded-lg">
						<img src="%s" alt="Inspection photo"
							class="w-16 h-16 rounded-lg object-cover border border-gray-200 shadow-sm hover:shadow-md transition-shadow"
							loading="lazy"/>
					</button>
					<button type="button"
						hx-delete="/api/inspections/%d/photo/%d"
						hx-target="#%s"
						hx-swap="outerHTML"
						hx-confirm="Delete this photo?"
						class="absolute -top-1.5 -right-1.5 w-5 h-5 bg-red-500 hover:bg-red-600 text-white rounded-full text-[10px] font-bold leading-none flex items-center justify-center shadow opacity-0 group-hover:opacity-100 active:opacity-100 focus:opacity-100 transition-opacity active:scale-90"
						title="Delete photo">&times;</button>
				</div>`,
				p.ID, galleryID, p.ID, thumbURL,
				inspectionID, p.ID,
				galleryID,
			))
		}
		sb.WriteString(`</div>`)
	}

	// ── Upload button + rapid shoot + spinner ──
	sb.WriteString(fmt.Sprintf(`
		<div class="flex items-center gap-2">
			<label for="%s"
				class="inline-flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium text-gray-500 bg-gray-50 hover:bg-gray-100 border border-gray-200 border-dashed rounded-lg cursor-pointer transition-colors active:scale-95">
				<svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
					<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 9a2 2 0 012-2h.93a2 2 0 001.664-.89l.812-1.22A2 2 0 0110.07 4h3.86a2 2 0 011.664.89l.812 1.22A2 2 0 0018.07 7H19a2 2 0 012 2v9a2 2 0 01-2 2H5a2 2 0 01-2-2V9z"/>
					<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 13a3 3 0 11-6 0 3 3 0 016 0z"/>
				</svg>
				<span>Add Photo</span>
			</label>
			<button type="button" onclick="startRapidShoot(%d, %d, true)"
				class="inline-flex items-center gap-1 px-2.5 py-1.5 text-xs font-medium text-duke-teal bg-duke-teal/5 hover:bg-duke-teal/10 border border-duke-teal/20 rounded-lg cursor-pointer transition-colors active:scale-95"
				title="Rapid Shoot: keep camera open between shots">
				<svg class="w-3.5 h-3.5" fill="currentColor" viewBox="0 0 20 20">
					<path d="M11.983 1.907a.75.75 0 00-1.292-.657l-8.5 9.5A.75.75 0 002.75 12h6.572l-1.305 6.093a.75.75 0 001.292.657l8.5-9.5A.75.75 0 0017.25 8h-6.572l1.305-6.093z"/>
				</svg>
				Rapid
			</button>
			<input type="file" id="%s" name="photo" accept="image/*" capture="environment"
				class="hidden"
				hx-post="/api/inspections/%d/adhoc/%d/photo"
				hx-target="#%s"
				hx-swap="outerHTML"
				hx-encoding="multipart/form-data"
				hx-trigger="change"
				hx-indicator="#upload-spinner-adhoc-%d"/>
			<div id="upload-spinner-adhoc-%d" class="htmx-indicator">
				<svg class="animate-spin w-4 h-4 text-duke-teal" fill="none" viewBox="0 0 24 24">
					<circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
					<path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
				</svg>
			</div>`,
		inputID,
		inspectionID, adhocItemID,
		inputID,
		inspectionID, adhocItemID, galleryID,
		adhocItemID, adhocItemID,
	))

	// Photo count badge
	if len(photos) > 0 {
		sb.WriteString(fmt.Sprintf(
			`<span class="text-[10px] text-gray-400">%d photo%s</span>`,
			len(photos), pluralS(len(photos)),
		))
	}

	sb.WriteString(`</div>`)  // close upload row
	sb.WriteString(`</div>`)  // close gallery container

	return sb.String()
}

// GET /api/inspections/:id/adhoc/:adhocId/photos — returns the HTMX photo gallery for an ad-hoc item.
// Used by rapid shoot to refresh the gallery after a burst session ends.
func (a *App) GetAdhocItemPhotosHTML(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	adhocItemID, err := strconv.Atoi(c.Param("adhocId"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid adhoc item id")
	}

	photos, err := a.db.GetInspectionPhotosByAdhocItem(ctx, inspectionID, adhocItemID)
	if err != nil {
		log.Printf("get adhoc item photos error: %v", err)
		return c.HTML(http.StatusOK, renderAdhocPhotoGalleryHTML(inspectionID, adhocItemID, nil))
	}

	return c.HTML(http.StatusOK, renderAdhocPhotoGalleryHTML(inspectionID, adhocItemID, photos))
}

// pluralS returns "s" if count != 1, for simple English pluralization.
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ─── HTMX Ad-hoc Photo Upload ────────────────────────────────────

// POST /api/inspections/:id/adhoc/:adhocId/photo — HTMX multipart upload for ad-hoc items.
// Returns the complete ad-hoc photo gallery HTML fragment.
func (a *App) UploadAdhocItemPhotoHTMX(c echo.Context) error {
	ctx := c.Request().Context()

	inspectionID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid inspection id")
	}

	adhocItemID, err := strconv.Atoi(c.Param("adhocId"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid adhoc item id")
	}

	// Read uploaded file — field name is "photo" (from the HTMX file input)
	file, err := c.FormFile("photo")
	if err != nil {
		// No file selected — return existing gallery unchanged
		photos, _ := a.db.GetInspectionPhotosByAdhocItem(ctx, inspectionID, adhocItemID)
		return c.HTML(http.StatusOK, renderAdhocPhotoGalleryHTML(inspectionID, adhocItemID, photos))
	}

	src, err := file.Open()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "cannot open file")
	}
	defer src.Close()

	data, err := io.ReadAll(src)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "cannot read file")
	}

	// Validate size
	maxBytes := a.config.MaxUploadMB * 1024 * 1024
	if len(data) > maxBytes || len(data) == 0 {
		photos, _ := a.db.GetInspectionPhotosByAdhocItem(ctx, inspectionID, adhocItemID)
		return c.HTML(http.StatusOK, renderAdhocPhotoGalleryHTML(inspectionID, adhocItemID, photos))
	}

	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext == "" || !isAllowedExt(ext) {
		ext = ".jpg"
	}

	// Process image (orient, thumbnail, EXIF)
	processed, _ := ProcessUpload(data)

	uniqueName := uuid.New().String()[:32] + ext
	dateDir := time.Now().Format("2006/01/02")

	photoPath := filepath.Join(a.config.StoragePath, "inspections", dateDir, uniqueName)
	thumbPath := filepath.Join(a.config.ThumbPath, "inspections", dateDir, uniqueName)

	if processed.Processed {
		quality := 95
		if ext == ".png" {
			quality = 0
		}
		if err := SaveImage(processed.Image, photoPath, quality); err != nil {
			log.Printf("save adhoc inspection photo error: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "save failed")
		}
		thumbQuality := 80
		if ext == ".png" {
			thumbQuality = 0
		}
		if err := SaveImage(processed.Thumb, thumbPath, thumbQuality); err != nil {
			log.Printf("save adhoc inspection thumb error: %v", err)
			thumbPath = ""
		}
	} else {
		if err := SaveRaw(data, photoPath); err != nil {
			log.Printf("save raw adhoc inspection photo error: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "save failed")
		}
		thumbPath = ""
	}

	var thumbPathPtr *string
	if thumbPath != "" {
		thumbPathPtr = &thumbPath
	}
	fileSize := len(data)
	var width, height *int
	if processed.Processed {
		w, h := processed.Width, processed.Height
		width, height = &w, &h
	}

	photo := &InspectionPhoto{
		InspectionID: inspectionID,
		AdhocItemID:  &adhocItemID,
		Filename:     uniqueName,
		StoragePath:  photoPath,
		ThumbPath:    thumbPathPtr,
		FileSize:     &fileSize,
		Width:        width,
		Height:       height,
		Lat:          processed.EXIF.Lat,
		Lng:          processed.EXIF.Lng,
	}

	if err := a.db.InsertInspectionPhoto(ctx, photo); err != nil {
		log.Printf("insert adhoc inspection photo error: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "database error")
	}

	log.Printf("adhoc inspection photo uploaded (HTMX): id=%d inspection=%d adhoc_item=%d", photo.ID, inspectionID, adhocItemID)

	// Return complete updated gallery
	photos, _ := a.db.GetInspectionPhotosByAdhocItem(ctx, inspectionID, adhocItemID)
	return c.HTML(http.StatusOK, renderAdhocPhotoGalleryHTML(inspectionID, adhocItemID, photos))
}
