package main

import (
	"context"
	"fmt"
	"log"
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
	DatabaseURL string
	StoragePath string
	ThumbPath   string
	BaseURL     string
	MaxUploadMB int
	Port        int
}

// App is the main application container.
type App struct {
	config Config
	db     *DB
}

func loadConfig() Config {
	port := 4010
	if p := os.Getenv("PORT"); p != "" {
		port, _ = strconv.Atoi(p)
	}
	maxMB := 50
	if m := os.Getenv("MAX_UPLOAD_MB"); m != "" {
		maxMB, _ = strconv.Atoi(m)
	}
	return Config{
		DatabaseURL: getEnv("DATABASE_URL", "postgres://trevor@localhost:5432/dukecam"),
		StoragePath: getEnv("STORAGE_PATH", "/data/photos"),
		ThumbPath:   getEnv("THUMB_PATH", "/data/thumbs"),
		BaseURL:     getEnv("BASE_URL", "https://dukecam.thomasduke.io"),
		MaxUploadMB: maxMB,
		Port:        port,
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (a *App) registerRoutes(e *echo.Echo) {
	// Pages
	e.GET("/", a.HomePage)
	e.GET("/p/:slug", a.ProjectPage)
	e.GET("/share/:slug", a.SharePage)
	e.GET("/admin", a.AdminPage)
	e.GET("/why", a.WhyPage)
	e.GET("/why/:filename", a.WhyAssets)

	// API — upload returns JSON (for JS engine), admin returns HTML fragments (HTMX)
	e.POST("/api/upload", a.UploadPhoto)
	e.GET("/api/photos/:slug", a.GetPhotos)
	e.PATCH("/api/photo/:id", a.UpdatePhoto)
	e.POST("/api/photo/:id/rotate", a.RotatePhoto)

	// Admin API (HTMX)
	e.POST("/api/admin/project", a.CreateProject)
	e.POST("/api/admin/worker", a.CreateWorker)
	e.POST("/api/admin/project/:id/toggle", a.ToggleProject)
	e.POST("/api/admin/worker/:id/toggle", a.ToggleWorker)
	e.GET("/api/admin/project/:id/qr", a.ProjectQR)

	// Media
	e.GET("/media/photo/:slug/:filename", a.ServePhoto)
	e.GET("/media/thumb/:slug/:filename", a.ServeThumb)
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

	// Ensure storage directories
	os.MkdirAll(cfg.StoragePath, 0755)
	os.MkdirAll(cfg.ThumbPath, 0755)

	app := &App{config: cfg, db: db}

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

	app.registerRoutes(e)

	// Graceful start + shutdown
	go func() {
		addr := fmt.Sprintf(":%d", cfg.Port)
		log.Printf("DukeCam starting on %s", addr)
		if err := e.Start(addr); err != nil {
			log.Printf("server stopped: %v", err)
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
