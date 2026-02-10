package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	qrcode "github.com/skip2/go-qrcode"
)

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

	log.Printf("upload: project=%s file=%s size=%d worker=%v",
		project.Slug, file.Filename, len(data), workerIDStr)

	// Determine file extension
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext == "" || !isAllowedExt(ext) {
		ext = ".jpg"
	}

	// Process image (decode, orient, thumbnail, EXIF)
	processed, err := ProcessUpload(data)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid image"})
	}

	// Generate unique filename and date-based directory
	uniqueName := uuid.New().String()[:32] + ext
	dateDir := time.Now().Format("2006/01/02")

	photoPath := filepath.Join(a.config.StoragePath, project.Slug, dateDir, uniqueName)
	thumbPath := filepath.Join(a.config.ThumbPath, project.Slug, dateDir, uniqueName)

	if processed.Processed {
		// Save processed image
		if ext == ".png" {
			if err := SaveImage(processed.Image, photoPath, 0); err != nil {
				log.Printf("save photo error: %v", err)
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": "save failed"})
			}
		} else {
			if err := SaveImage(processed.Image, photoPath, 85); err != nil {
				log.Printf("save photo error: %v", err)
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": "save failed"})
			}
		}

		// Save thumbnail
		thumbQuality := 80
		if ext == ".png" {
			thumbQuality = 0
		}
		if err := SaveImage(processed.Thumb, thumbPath, thumbQuality); err != nil {
			log.Printf("save thumb error: %v", err)
			// Non-fatal — continue without thumb
			thumbPath = ""
		}
	} else {
		// Can't process — save raw bytes
		if err := SaveRaw(data, photoPath); err != nil {
			log.Printf("save raw error: %v", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "save failed"})
		}
		thumbPath = "" // no thumbnail for unprocessable formats
	}

	// Validate tag
	if tag != "" && tag != "progress" && tag != "before" && tag != "after" && tag != "issue" {
		tag = ""
	}

	// Nullable fields
	var captionPtr, tagPtr, workerNamePtr, batchPtr, origFilename, thumbPathPtr *string
	if caption != "" {
		captionPtr = &caption
	}
	if tag != "" {
		tagPtr = &tag
	}
	if workerID == nil && workerName != "" {
		workerNamePtr = &workerName
	}
	if batchID != "" {
		batchPtr = &batchID
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
		ProjectID:          projectID,
		WorkerID:           workerID,
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
	}

	if err := a.db.InsertPhoto(ctx, photo); err != nil {
		log.Printf("insert photo error: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "database error"})
	}

	log.Printf("upload complete: id=%d file=%s project=%s", photo.ID, uniqueName, project.Slug)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"id":       photo.ID,
		"filename": uniqueName,
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
	case ".jpg", ".jpeg", ".png", ".heic", ".webp":
		return true
	}
	return false
}
