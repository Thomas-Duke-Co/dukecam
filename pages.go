package main

import (
	"encoding/json"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/labstack/echo/v4"
)

// ─── Day-grouped photos ─────────────────────────────────────────

type DayGroup struct {
	Key    string // "2025-01-15"
	Label  string // "Wednesday, January 15, 2025"
	Photos []PhotoWithWorker
}

// LightboxPhoto is the JSON structure embedded in the page for the JS lightbox.
type LightboxPhoto struct {
	ID      int    `json:"id"`
	URL     string `json:"url"`
	Caption string `json:"caption"`
	Worker  string `json:"worker"`
	Tag     string `json:"tag"`
	Date    string `json:"date"`
}

func groupPhotosByDay(photos []PhotoWithWorker) []DayGroup {
	groupMap := make(map[string]*DayGroup)
	var order []string

	for _, p := range photos {
		key := p.UploadedAt.Format("2006-01-02")
		label := p.UploadedAt.Format("Monday, January 2, 2006")

		if _, exists := groupMap[key]; !exists {
			groupMap[key] = &DayGroup{Key: key, Label: label}
			order = append(order, key)
		}
		groupMap[key].Photos = append(groupMap[key].Photos, p)
	}

	var groups []DayGroup
	for _, key := range order {
		groups = append(groups, *groupMap[key])
	}
	return groups
}

func makeLightboxData(photos []PhotoWithWorker, slug string) template.JS {
	var lb []LightboxPhoto
	for _, p := range photos {
		caption := ""
		if p.Caption != nil {
			caption = *p.Caption
		}
		tag := ""
		if p.Tag != nil {
			tag = *p.Tag
		}
		lb = append(lb, LightboxPhoto{
			ID:      p.ID,
			URL:     "/media/photo/" + slug + "/" + p.Filename,
			Caption: caption,
			Worker:  p.DisplayName(),
			Tag:     tag,
			Date:    p.UploadedAt.Format("Jan 2, 2006 3:04 PM"),
		})
	}
	b, _ := json.Marshal(lb)
	return template.JS(b)
}

// ─── Page Handlers ───────────────────────────────────────────────

func (a *App) HomePage(c echo.Context) error {
	summaries, err := a.db.ListProjectSummaries(c.Request().Context())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to load projects")
	}

	return c.Render(http.StatusOK, "home", map[string]interface{}{
		"ProjectData": summaries,
	})
}

func (a *App) ProjectPage(c echo.Context) error {
	slug := c.Param("slug")
	ctx := c.Request().Context()

	project, err := a.db.GetProjectBySlug(ctx, slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}

	workers, err := a.db.ListActiveWorkers(ctx)
	if err != nil {
		workers = nil
	}

	photos, err := a.db.GetPhotosForProject(ctx, project.ID)
	if err != nil {
		photos = nil
	}

	dayGroups := groupPhotosByDay(photos)
	photosJSON := makeLightboxData(photos, slug)

	return c.Render(http.StatusOK, "project", map[string]interface{}{
		"Project":    project,
		"Workers":    workers,
		"Photos":     photos,
		"DayGroups":  dayGroups,
		"PhotosJSON": photosJSON,
		"PhotoCount": len(photos),
	})
}

func (a *App) SharePage(c echo.Context) error {
	slug := c.Param("slug")
	ctx := c.Request().Context()

	project, err := a.db.GetProjectBySlug(ctx, slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}

	photos, err := a.db.GetPhotosForProject(ctx, project.ID)
	if err != nil {
		photos = nil
	}

	dayGroups := groupPhotosByDay(photos)
	photosJSON := makeLightboxData(photos, slug)

	// First photo for OG meta tag
	var firstThumbURL string
	if len(photos) > 0 {
		firstThumbURL = a.config.BaseURL + "/media/thumb/" + slug + "/" + photos[0].Filename
	}

	return c.Render(http.StatusOK, "share", map[string]interface{}{
		"Project":       project,
		"Photos":        photos,
		"DayGroups":     dayGroups,
		"PhotosJSON":    photosJSON,
		"PhotoCount":    len(photos),
		"BaseURL":       a.config.BaseURL,
		"FirstThumbURL": firstThumbURL,
	})
}

func (a *App) AdminPage(c echo.Context) error {
	ctx := c.Request().Context()

	projects, err := a.db.ListAllProjects(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to load projects")
	}

	// Sort: active first, then by name
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Active != projects[j].Active {
			return projects[i].Active
		}
		return projects[i].Name < projects[j].Name
	})

	workers, err := a.db.ListAllWorkers(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to load workers")
	}

	return c.Render(http.StatusOK, "admin", map[string]interface{}{
		"Projects": projects,
		"Workers":  workers,
		"BaseURL":  a.config.BaseURL,
	})
}

func (a *App) WhyPage(c echo.Context) error {
	return c.File("comparison/comparison.html")
}

func (a *App) WhyAssets(c echo.Context) error {
	filename := c.Param("filename")
	path := filepath.Join("comparison", filename)
	if _, err := os.Stat(path); err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	return c.File(path)
}
