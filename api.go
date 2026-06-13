package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	qrcode "github.com/skip2/go-qrcode"
)

// photoMeta carries the optional, per-upload attributes shared by the worker
// upload path (/api/upload) and the PropertyOS ingest path
// (/api/propertyos/photos). The PropertyOS fields are nil for worker uploads.
type photoMeta struct {
	WorkerID   *int
	WorkerName string // free-text override when WorkerID is nil
	Caption    string
	Tag        string
	BatchID    string

	// PropertyOS linkage (claudecode-u61f)
	BuildingID *int
	UnitID     *int
	TenantID   *int
	TenantName *string
	Scope      *string
}

// storeUploadedPhoto is the shared save core: validate size, process the image
// (decode/orient/thumbnail/EXIF), persist the file + thumb under the project's
// slug, and insert the photos row. Returns (photo, 0, "") on success or
// (nil, httpStatus, message) on a handled failure so each caller can shape its
// own response. Extracted from UploadPhoto so the PropertyOS ingest route reuses
// the exact same pipeline.
func (a *App) storeUploadedPhoto(c echo.Context, project *Project, file *multipart.FileHeader, meta photoMeta) (*Photo, int, string) {
	ctx := c.Request().Context()

	src, err := file.Open()
	if err != nil {
		return nil, http.StatusBadRequest, "cannot open file"
	}
	defer src.Close()

	data, err := io.ReadAll(src)
	if err != nil {
		return nil, http.StatusBadRequest, "cannot read file"
	}

	maxBytes := a.config.MaxUploadMB * 1024 * 1024
	if len(data) > maxBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Sprintf("file too large (max %dMB)", a.config.MaxUploadMB)
	}
	if len(data) == 0 {
		return nil, http.StatusBadRequest, "empty file"
	}

	// Determine file extension
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext == "" || !isAllowedExt(ext) {
		ext = ".jpg"
	}
	// HEIC/HEIF: decoded to image.Image then saved as JPEG — remap extension so
	// the stored file is served as a renderable JPEG.
	if ext == ".heic" || ext == ".heif" {
		ext = ".jpg"
	}

	processed, err := ProcessUpload(data)
	if err != nil {
		return nil, http.StatusBadRequest, "invalid image"
	}

	uniqueName := uuid.New().String()[:32] + ext
	dateDir := time.Now().Format("2006/01/02")
	photoPath := filepath.Join(a.config.StoragePath, project.Slug, dateDir, uniqueName)
	thumbPath := filepath.Join(a.config.ThumbPath, project.Slug, dateDir, uniqueName)

	if processed.Processed {
		quality := 95
		if ext == ".png" {
			quality = 0
		}
		if err := SaveImage(processed.Image, photoPath, quality); err != nil {
			log.Printf("save photo error: %v", err)
			return nil, http.StatusInternalServerError, "save failed"
		}
		thumbQuality := 80
		if ext == ".png" {
			thumbQuality = 0
		}
		if err := SaveImage(processed.Thumb, thumbPath, thumbQuality); err != nil {
			log.Printf("save thumb error: %v", err)
			thumbPath = "" // non-fatal — continue without thumb
		}
	} else {
		if err := SaveRaw(data, photoPath); err != nil {
			log.Printf("save raw error: %v", err)
			return nil, http.StatusInternalServerError, "save failed"
		}
		thumbPath = "" // no thumbnail for unprocessable formats
	}

	// Validate tag
	tag := meta.Tag
	if tag != "" && tag != "progress" && tag != "before" && tag != "after" && tag != "issue" {
		tag = ""
	}

	// Nullable fields
	var captionPtr, tagPtr, workerNamePtr, batchPtr, origFilename, thumbPathPtr *string
	if meta.Caption != "" {
		captionPtr = &meta.Caption
	}
	if tag != "" {
		tagPtr = &tag
	}
	if meta.WorkerID == nil && meta.WorkerName != "" {
		workerNamePtr = &meta.WorkerName
	}
	if meta.BatchID != "" {
		batchPtr = &meta.BatchID
	}
	if file.Filename != "" {
		fn := file.Filename
		origFilename = &fn
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

	photo := &Photo{
		ProjectID:          project.ID,
		WorkerID:           meta.WorkerID,
		WorkerNameOverride: workerNamePtr,
		Filename:           uniqueName,
		OriginalFilename:   origFilename,
		Caption:            captionPtr,
		Tag:                tagPtr,
		Lat:                processed.EXIF.Lat,
		Lng:                processed.EXIF.Lng,
		TakenAt:            processed.EXIF.TakenAt,
		FileSize:           &fileSize,
		Width:              width,
		Height:             height,
		StoragePath:        photoPath,
		ThumbPath:          thumbPathPtr,
		UploadBatch:        batchPtr,
		BuildingID:         meta.BuildingID,
		UnitID:             meta.UnitID,
		TenantID:           meta.TenantID,
		TenantName:         meta.TenantName,
		Scope:              meta.Scope,
	}

	if err := a.db.InsertPhoto(ctx, photo); err != nil {
		log.Printf("insert photo error: %v", err)
		return nil, http.StatusInternalServerError, "database error"
	}
	return photo, 0, ""
}

// ─── Upload API (JSON response — consumed by upload.js) ──────────

func (a *App) UploadPhoto(c echo.Context) error {
	ctx := c.Request().Context()

	// Parse form fields
	projectIDStr := c.FormValue("project_id")
	projectID, err := strconv.Atoi(projectIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid project_id"})
	}

	workerIDStr := c.FormValue("worker_id")
	workerName := c.FormValue("worker_name")
	caption := c.FormValue("caption")
	tag := c.FormValue("tag")
	batchID := c.FormValue("batch_id")

	var workerID *int
	if workerIDStr != "" {
		if wid, err := strconv.Atoi(workerIDStr); err == nil {
			workerID = &wid
		}
	}

	// Validate project
	project, err := a.db.GetProjectByID(ctx, projectID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid project"})
	}

	// Read uploaded file
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "no file uploaded"})
	}

	log.Printf("upload: project=%s file=%s worker=%v", project.Slug, file.Filename, workerIDStr)

	photo, status, errMsg := a.storeUploadedPhoto(c, project, file, photoMeta{
		WorkerID:   workerID,
		WorkerName: workerName,
		Caption:    caption,
		Tag:        tag,
		BatchID:    batchID,
	})
	if status != 0 {
		return c.JSON(status, map[string]string{"error": errMsg})
	}

	log.Printf("upload complete: id=%d file=%s project=%s", photo.ID, photo.Filename, project.Slug)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"id":       photo.ID,
		"filename": photo.Filename,
		"status":   "ok",
	})
}

// ─── Photos API (JSON) ──────────────────────────────────────────

func (a *App) GetPhotos(c echo.Context) error {
	slug := c.Param("slug")
	ctx := c.Request().Context()

	project, err := a.db.GetProjectBySlug(ctx, slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	photos, err := a.db.GetPhotosForProjectPaginated(ctx, project.ID, offset, limit)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	type photoJSON struct {
		ID         int     `json:"id"`
		Filename   string  `json:"filename"`
		Caption    *string `json:"caption"`
		Tag        *string `json:"tag"`
		Worker     string  `json:"worker"`
		UploadedAt string  `json:"uploaded_at,omitempty"`
		TakenAt    string  `json:"taken_at,omitempty"`
		ThumbURL   string  `json:"thumb_url"`
		FullURL    string  `json:"full_url"`
	}

	var result []photoJSON
	for _, p := range photos {
		pj := photoJSON{
			ID:       p.ID,
			Filename: p.Filename,
			Caption:  p.Caption,
			Tag:      p.Tag,
			Worker:   p.DisplayName(),
			ThumbURL: "/media/thumb/" + slug + "/" + p.Filename,
			FullURL:  "/media/photo/" + slug + "/" + p.Filename,
		}
		pj.UploadedAt = p.UploadedAt.Format(time.RFC3339)
		if p.TakenAt != nil {
			pj.TakenAt = p.TakenAt.Format(time.RFC3339)
		}
		result = append(result, pj)
	}

	return c.JSON(http.StatusOK, result)
}

// ─── Admin API (HTMX — returns HTML fragments) ──────────────────

func (a *App) CreateProject(c echo.Context) error {
	name := c.FormValue("name")
	slug := c.FormValue("slug")
	address := c.FormValue("address")
	description := c.FormValue("description")

	if name == "" || slug == "" {
		return c.String(http.StatusBadRequest, "Name and slug are required")
	}

	var addrPtr, descPtr *string
	if address != "" {
		addrPtr = &address
	}
	if description != "" {
		descPtr = &description
	}

	project, err := a.db.CreateProject(c.Request().Context(), name, slug, addrPtr, descPtr)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to create project: "+err.Error())
	}

	// Return HTML fragment for HTMX
	ap := AdminProject{Project: *project, PhotoCount: 0}
	return renderFragment(c, "project-item", ap)
}

// RegisterWorker is called from the project page when a user types a custom name.
// It finds an existing worker with that name (case-insensitive) or creates a new one,
// returning the worker ID so the frontend can switch from the free-text input to a real dropdown entry.
func (a *App) RegisterWorker(c echo.Context) error {
	var body struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
	}
	name := strings.TrimSpace(body.Name)
	if len(name) < 2 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name too short"})
	}
	if len(name) > 100 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name too long"})
	}

	ctx := c.Request().Context()
	worker, err := a.db.FindWorkerByName(ctx, name)
	if err != nil {
		shortCode := uuid.New().String()[:8]
		worker, err = a.db.CreateWorker(ctx, name, shortCode)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to register"})
		}
		log.Printf("worker auto-registered: id=%d name=%q", worker.ID, worker.Name)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"id":   worker.ID,
		"name": worker.Name,
	})
}

func (a *App) CreateWorker(c echo.Context) error {
	name := c.FormValue("name")
	if name == "" {
		return c.String(http.StatusBadRequest, "Name is required")
	}

	shortCode := uuid.New().String()[:8]
	worker, err := a.db.CreateWorker(c.Request().Context(), name, shortCode)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to create worker: "+err.Error())
	}

	return renderFragment(c, "worker-item", worker)
}

func (a *App) ToggleProject(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	project, err := a.db.ToggleProjectActive(c.Request().Context(), id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	count := a.db.PhotoCountForProject(c.Request().Context(), id)
	ap := AdminProject{Project: *project, PhotoCount: count}
	return renderFragment(c, "project-item", ap)
}

func (a *App) ToggleWorker(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	worker, err := a.db.ToggleWorkerActive(c.Request().Context(), id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	return renderFragment(c, "worker-item", worker)
}

func (a *App) ProjectQR(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	project, err := a.db.GetProjectByID(c.Request().Context(), id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	url := a.config.BaseURL + "/p/" + project.Slug
	png, err := qrcode.Encode(url, qrcode.Medium, 256)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "QR generation failed")
	}

	c.Response().Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="qr-%s.png"`, project.Slug))
	return c.Blob(http.StatusOK, "image/png", png)
}

// ─── Update Photo Annotations (JSON) ─────────────────────────────

func (a *App) UpdatePhoto(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid photo id"})
	}

	var body struct {
		Caption *string `json:"caption"`
		Tag     *string `json:"tag"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
	}

	// Validate tag enum
	if body.Tag != nil && *body.Tag != "" {
		switch *body.Tag {
		case "progress", "before", "after", "issue":
		default:
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid tag value"})
		}
	}

	// Validate photo exists
	if _, err := a.db.GetPhotoByID(ctx, id); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "photo not found"})
	}

	if err := a.db.UpdatePhotoAnnotations(ctx, id, body.Caption, body.Tag); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "update failed"})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// ─── Rotate Photo ────────────────────────────────────────────────

func (a *App) RotatePhoto(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid photo id"})
	}

	var body struct {
		Direction string `json:"direction"` // "cw", "ccw", or "180"
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
	}
	if body.Direction == "" {
		body.Direction = "cw"
	}
	if body.Direction != "cw" && body.Direction != "ccw" && body.Direction != "180" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "direction must be cw, ccw, or 180"})
	}

	photo, err := a.db.GetPhotoByID(ctx, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "photo not found"})
	}

	thumbPath := ""
	if photo.ThumbPath != nil {
		thumbPath = *photo.ThumbPath
	}

	newW, newH, err := RotateFile(photo.StoragePath, thumbPath, body.Direction)
	if err != nil {
		log.Printf("rotate error: id=%d err=%v", id, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "rotation failed"})
	}

	// Update dimensions in DB
	a.db.UpdatePhotoDimensions(ctx, id, newW, newH)

	log.Printf("rotated photo: id=%d direction=%s new=%dx%d", id, body.Direction, newW, newH)
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// ─── Helpers ─────────────────────────────────────────────────────

func renderFragment(c echo.Context, name string, data interface{}) error {
	renderer := c.Echo().Renderer.(*Renderer)
	var buf bytes.Buffer
	if err := renderer.RenderFragment(&buf, name, data); err != nil {
		return c.String(http.StatusInternalServerError, "render error: "+err.Error())
	}
	return c.HTML(http.StatusOK, buf.String())
}

func isAllowedExt(ext string) bool {
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".heic", ".heif":
		return true
	}
	return false
}
