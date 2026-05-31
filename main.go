package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// Config holds all application configuration from environment variables.
type Config struct {
	DatabaseURL     string
	StoragePath     string
	ThumbPath       string
	BaseURL         string
	MaxUploadMB     int
	Port            int
	FyxtURL         string
	FyxtAPIKey      string
	FyxtSchema      string
	PropertyOSURL   string
	PropertyOSToken string
}

// App is the main application container.
type App struct {
	config     Config
	db         *DB
	fyxt       *FyxtClient
	propertyOS *PropertyOSClient
}

func loadConfig() Config {
	port := 4010
	if p := os.Getenv("PORT"); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil || v < 1 || v > 65535 {
			log.Fatalf("invalid PORT %q: must be an integer in 1..65535", p)
		}
		port = v
	}
	maxMB := 50
	if m := os.Getenv("MAX_UPLOAD_MB"); m != "" {
		v, err := strconv.Atoi(m)
		if err != nil || v < 1 {
			log.Fatalf("invalid MAX_UPLOAD_MB %q: must be a positive integer", m)
		}
		maxMB = v
	}
	return Config{
		DatabaseURL: getEnv("DATABASE_URL", "postgres://trevor@localhost:5432/dukecam"),
		StoragePath: getEnv("STORAGE_PATH", "/data/photos"),
		ThumbPath:   getEnv("THUMB_PATH", "/data/thumbs"),
		BaseURL:     getEnv("BASE_URL", "https://dukecam.thomasduke.io"),
		MaxUploadMB: maxMB,
		Port:        port,
		FyxtURL:         getEnv("FYXT_URL", "https://open.apifyxt.com"),
		FyxtAPIKey:      getEnv("FYXT_API_KEY", ""),
		FyxtSchema:      getEnv("FYXT_SCHEMA", "thomasduke"),
		PropertyOSURL:   getEnv("PROPERTYOS_URL", "http://localhost:4125"),
		PropertyOSToken: getEnv("PROPERTYOS_TOKEN", ""),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (a *App) registerRoutes(e *echo.Echo) {
	// Health check (used by offline sync heartbeat)
	e.GET("/api/health", func(c echo.Context) error {
		return c.NoContent(204)
	})
	e.HEAD("/api/health", func(c echo.Context) error {
		return c.NoContent(204)
	})

	// Pages
	e.GET("/", a.HomePage)
	e.GET("/p/:slug", a.ProjectPage)
	e.GET("/share/:slug", a.SharePage)
	e.GET("/admin", a.AdminPage)
	e.GET("/why", a.WhyPage)
	e.GET("/why/:filename", a.WhyAssets)
	e.GET("/offline", a.OfflinePage)

	// API — upload returns JSON (for JS engine), admin returns HTML fragments (HTMX)
	e.POST("/api/upload", a.UploadPhoto)
	e.GET("/api/photos/:slug", a.GetPhotos)
	e.PATCH("/api/photo/:id", a.UpdatePhoto)
	e.POST("/api/photo/:id/rotate", a.RotatePhoto)

	// Worker self-registration (called from project page when user types a custom name)
	e.POST("/api/workers/register", a.RegisterWorker)

	// Admin API (HTMX)
	e.POST("/api/admin/project", a.CreateProject)
	e.POST("/api/admin/worker", a.CreateWorker)
	e.POST("/api/admin/project/:id/toggle", a.ToggleProject)
	e.POST("/api/admin/worker/:id/toggle", a.ToggleWorker)
	e.GET("/api/admin/project/:id/qr", a.ProjectQR)

	// Media
	e.GET("/media/photo/:slug/:filename", a.ServePhoto)
	e.GET("/media/thumb/:slug/:filename", a.ServeThumb)

	// Inspection Flow — Pages
	e.GET("/inspections/new", a.NewInspectionPage)
	e.GET("/inspection/:id", a.InspectionConductPage)
	e.GET("/inspection/:id/print", a.PrintInspectionPage)
	e.GET("/share/inspection/:token", a.ShareInspectionPage)

	// Inspection Conduct API (HTMX — checklist status updates)
	e.POST("/api/inspections/create", a.CreateInspectionHandler)
	e.POST("/api/inspections/:id/share", a.GenerateShareLink)
	e.POST("/api/inspections/submit", a.SubmitInspectionHandler) // JSON — offline sync single submission
	e.POST("/api/inspections/sync", a.BatchSyncHandler)         // JSON — offline sync batch (idempotent)
	e.POST("/api/inspections/:id/item/:itemId/status", a.UpdateItemStatusHandler)
	e.POST("/api/inspections/:id/complete", a.CompleteInspectionHandler)
	e.POST("/api/inspections/:id/reopen", a.ReopenInspectionHandler)
	e.POST("/api/inspections/:id/work-order", a.CreateFyxtWorkOrderHandler)

	// Ad-hoc Items API (HTMX — add/update/delete items during inspection)
	e.POST("/api/inspections/:id/adhoc", a.CreateAdhocItemHandler)
	e.POST("/api/inspections/:id/adhoc/:adhocId/status", a.UpdateAdhocItemStatusHandler)
	e.POST("/api/inspections/:id/adhoc/:adhocId/photo", a.UploadAdhocItemPhotoHTMX)
	e.GET("/api/inspections/:id/adhoc/:adhocId/photos", a.GetAdhocItemPhotosHTML)
	e.DELETE("/api/inspections/:id/adhoc/:adhocId", a.DeleteAdhocItemHandler)

	// Inspection Photos — HTMX (per-item upload/delete returns HTML gallery fragment)
	e.POST("/api/inspections/:id/item/:itemId/photo", a.UploadInspectionItemPhotoHTMX)
	e.DELETE("/api/inspections/:id/photo/:photoId", a.DeleteInspectionItemPhotoHTMX)
	e.GET("/api/inspections/:id/item/:itemId/photos", a.GetItemPhotosHTML)

	// Inspection Photos — JSON API (general upload + serve + delete)
	e.POST("/api/inspections/:id/items/:itemId/photos", a.UploadInspectionItemPhoto)
	e.GET("/api/inspections/:id/items/:itemId/photos", a.GetInspectionItemPhotos)
	e.POST("/api/inspections/:id/photos", a.UploadInspectionPhoto)
	e.DELETE("/api/inspections/:id/photos/:photoId", a.DeleteInspectionPhotoHandler)
	e.GET("/api/inspections/photos/:id", a.ServeInspectionPhoto)
	e.GET("/api/inspections/photos/:id/thumb", a.ServeInspectionPhotoThumb)

	// Inspection Flow API (JSON — for inspector UI + IndexedDB offline cache)
	e.GET("/api/inspections/filter", a.FilterInspectionsHTML)     // HTMX — filter inspection list by property
	e.GET("/api/inspections/properties", a.ListProperties)
	e.GET("/api/inspections/properties/search", a.SearchPropertiesHTML) // HTMX — returns HTML fragment
	e.GET("/api/inspections/properties/:id", a.GetPropertyDetail)
	e.GET("/api/inspections/properties/:id/units", a.GetPropertyUnitsHTML) // HTMX — returns HTML <option> fragment
	e.GET("/api/inspections/templates", a.ListAvailableTemplates)
	e.GET("/api/inspections/templates/:id", a.GetTemplateChecklist)
	e.GET("/api/inspections/templates/:id/preview", a.GetTemplatePreviewHTML) // HTMX — template section/item preview
	e.GET("/api/inspections/inspectors", a.ListInspectors)
	e.GET("/api/inspections/inspectors/search", a.SearchInspectors) // HTMX — returns HTML fragment

	// Inspection Admin Pages
	e.GET("/admin/inspections", a.InspectionTemplatesPage)
	e.GET("/admin/inspections/:id", a.InspectionTemplateDetailPage)

	// Inspection Admin API (HTMX — returns HTML fragments)
	e.POST("/api/admin/inspection-template", a.CreateInspectionTemplate)
	e.PUT("/api/admin/inspection-template/:id", a.UpdateInspectionTemplate)
	e.POST("/api/admin/inspection-template/:id/toggle", a.ToggleInspectionTemplate)
	e.POST("/api/admin/inspection-template/:id/duplicate", a.DuplicateInspectionTemplate)
	e.DELETE("/api/admin/inspection-template/:id", a.DeleteInspectionTemplate)
	e.POST("/api/admin/inspection-template/:id/category", a.CreateTemplateCategory)
	e.PUT("/api/admin/inspection-category/:id", a.UpdateTemplateCategory)
	e.DELETE("/api/admin/inspection-category/:id", a.DeleteTemplateCategory)
	e.POST("/api/admin/inspection-category/:id/item", a.CreateTemplateItem)
	e.PUT("/api/admin/inspection-item/:id", a.UpdateTemplateItem)
	e.DELETE("/api/admin/inspection-item/:id", a.DeleteTemplateItem)
}

func main() {
	cfg := loadConfig()

	// Database
	db, err := NewDB(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer db.Close()

	if err := db.CreateTables(context.Background()); err != nil {
		log.Fatalf("table creation failed: %v", err)
	}
	if err := db.CreateInspectionTables(context.Background()); err != nil {
		log.Fatalf("inspection table creation failed: %v", err)
	}
	if err := db.CreateInspectionRuntimeTables(context.Background()); err != nil {
		log.Fatalf("inspection runtime table creation failed: %v", err)
	}
	if err := db.CreateAdhocItemsTables(context.Background()); err != nil {
		log.Fatalf("adhoc items table creation failed: %v", err)
	}
	if err := db.CreateInspectionPhotosTables(context.Background()); err != nil {
		log.Fatalf("inspection photos table creation failed: %v", err)
	}
	if err := db.CreateSyncTables(context.Background()); err != nil {
		log.Fatalf("sync tables creation failed: %v", err)
	}
	if err := db.SeedDefaultInspectionTemplates(context.Background()); err != nil {
		log.Fatalf("inspection template seeding failed: %v", err)
	}
	// Migrations (idempotent column additions)
	if err := db.AddShareTokenColumn(context.Background()); err != nil {
		log.Printf("share_token migration warning: %v", err)
	}

	// Ensure storage directories
	os.MkdirAll(cfg.StoragePath, 0755)
	os.MkdirAll(cfg.ThumbPath, 0755)

	// PropertyOS API client (optional — gracefully degrades if not configured)
	pos := NewPropertyOSClient(PropertyOSConfig{
		BaseURL: cfg.PropertyOSURL,
		Token:   cfg.PropertyOSToken,
	})
	if !pos.IsConfigured() {
		log.Println("WARNING: PROPERTYOS_URL not set — property list will be unavailable")
	} else {
		log.Printf("PropertyOS client configured: %s", cfg.PropertyOSURL)
	}

	// Fyxt API client (optional — gracefully degrades if not configured)
	fyxt := NewFyxtClient(cfg.FyxtURL, cfg.FyxtAPIKey, cfg.FyxtSchema)
	if !fyxt.IsConfigured() {
		log.Println("WARNING: FYXT_API_KEY not set — inspector list will be unavailable")
	}

	app := &App{config: cfg, db: db, fyxt: fyxt, propertyOS: pos}

	// Echo
	e := echo.New()
	e.HideBanner = true
	e.Renderer = NewRenderer()

	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Format: "${time_rfc3339} ${method} ${uri} ${status} ${latency_human}\n",
	}))
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit(fmt.Sprintf("%dM", cfg.MaxUploadMB)))

	e.Static("/static", "static")

	// Service worker must be served from root for proper scope.
	// Disable caching: a stale /sw.js at any edge means PWAs can't see
	// new SW versions, which means they can't flush their precache.
	e.GET("/sw.js", func(c echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Response().Header().Set("Pragma", "no-cache")
		c.Response().Header().Set("Expires", "0")
		return c.File("static/sw.js")
	})

	app.registerRoutes(e)

	// Graceful start + shutdown. A real bind failure (port in use, permission)
	// must crash the process — otherwise it stays alive with no listener and
	// the container looks "up" while serving nothing. ErrServerClosed is the
	// expected signal from e.Shutdown below, so it is not fatal.
	go func() {
		addr := fmt.Sprintf(":%d", cfg.Port)
		log.Printf("DukeCam starting on %s", addr)
		if err := e.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server failed to start: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.Shutdown(ctx)
}
