package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── Models ──────────────────────────────────────────────────────

type Project struct {
	ID          int
	Name        string
	Slug        string
	Address     *string
	Description *string
	Active      bool
	CreatedAt   time.Time
}

type Worker struct {
	ID        int
	Name      string
	ShortCode string
	Active    bool
	CreatedAt time.Time
}

type Photo struct {
	ID                 int
	ProjectID          int
	WorkerID           *int
	WorkerNameOverride *string
	Filename           string
	OriginalFilename   *string
	Caption            *string
	Tag                *string
	Lat                *float64
	Lng                *float64
	TakenAt            *time.Time
	UploadedAt         time.Time
	FileSize           *int
	Width              *int
	Height             *int
	StoragePath        string
	ThumbPath          *string
	UploadBatch        *string
}

// PhotoWithWorker includes the resolved worker name from a JOIN.
type PhotoWithWorker struct {
	Photo
	WorkerName string
}

func (p *PhotoWithWorker) DisplayName() string {
	if p.WorkerName != "" {
		return p.WorkerName
	}
	if p.WorkerNameOverride != nil {
		return *p.WorkerNameOverride
	}
	return "Unknown"
}

// Aggregated project data for home page.
type ProjectSummary struct {
	Project       Project
	PhotoCount    int
	LastUpdated   *time.Time
	RecentPhotos  []Photo
	RecentWorkers []string
}

// Admin view of a project with photo count.
type AdminProject struct {
	Project
	PhotoCount int
}

// ─── Database ────────────────────────────────────────────────────

type DB struct {
	pool *pgxpool.Pool
}

func NewDB(ctx context.Context, connStr string) (*DB, error) {
	config, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	config.MaxConns = 10
	config.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}

	return &DB{pool: pool}, nil
}

func (db *DB) Close() {
	db.pool.Close()
}

func (db *DB) CreateTables(ctx context.Context) error {
	sql := `
	CREATE TABLE IF NOT EXISTS workers (
		id SERIAL PRIMARY KEY,
		name VARCHAR(200) NOT NULL,
		short_code VARCHAR(20) UNIQUE NOT NULL,
		active BOOLEAN NOT NULL DEFAULT true,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE TABLE IF NOT EXISTS projects (
		id SERIAL PRIMARY KEY,
		name VARCHAR(300) NOT NULL,
		slug VARCHAR(100) UNIQUE NOT NULL,
		address TEXT,
		description TEXT,
		active BOOLEAN NOT NULL DEFAULT true,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE TABLE IF NOT EXISTS photos (
		id SERIAL PRIMARY KEY,
		project_id INTEGER NOT NULL REFERENCES projects(id),
		worker_id INTEGER REFERENCES workers(id),
		worker_name_override VARCHAR(200),
		filename VARCHAR(500) NOT NULL,
		original_filename VARCHAR(500),
		caption TEXT,
		tag VARCHAR(50),
		lat DOUBLE PRECISION,
		lng DOUBLE PRECISION,
		taken_at TIMESTAMPTZ,
		uploaded_at TIMESTAMPTZ DEFAULT NOW(),
		file_size INTEGER,
		width INTEGER,
		height INTEGER,
		storage_path TEXT NOT NULL,
		thumb_path TEXT,
		upload_batch VARCHAR(50)
	);
	CREATE INDEX IF NOT EXISTS idx_photos_filename ON photos(filename);
	CREATE INDEX IF NOT EXISTS idx_photos_project_id ON photos(project_id);
	`
	_, err := db.pool.Exec(ctx, sql)
	return err
}

// ─── Project Queries ─────────────────────────────────────────────

func (db *DB) ListProjectSummaries(ctx context.Context) ([]ProjectSummary, error) {
	// Get active projects with counts
	rows, err := db.pool.Query(ctx, `
		SELECT p.id, p.name, p.slug, p.address, p.description, p.active, p.created_at,
		       COALESCE(s.cnt, 0), s.last_up
		FROM projects p
		LEFT JOIN (
			SELECT project_id, COUNT(*) as cnt, MAX(uploaded_at) as last_up
			FROM photos GROUP BY project_id
		) s ON p.id = s.project_id
		WHERE p.active = true
		ORDER BY s.last_up DESC NULLS LAST
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []ProjectSummary
	for rows.Next() {
		var ps ProjectSummary
		err := rows.Scan(
			&ps.Project.ID, &ps.Project.Name, &ps.Project.Slug,
			&ps.Project.Address, &ps.Project.Description,
			&ps.Project.Active, &ps.Project.CreatedAt,
			&ps.PhotoCount, &ps.LastUpdated,
		)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, ps)
	}

	// Fetch recent photos and workers for each project
	for i := range summaries {
		pid := summaries[i].Project.ID

		// Recent 4 photos
		prows, err := db.pool.Query(ctx, `
			SELECT id, filename, uploaded_at FROM photos
			WHERE project_id = $1 ORDER BY uploaded_at DESC LIMIT 4
		`, pid)
		if err == nil {
			for prows.Next() {
				var p Photo
				prows.Scan(&p.ID, &p.Filename, &p.UploadedAt)
				summaries[i].RecentPhotos = append(summaries[i].RecentPhotos, p)
			}
			prows.Close()
		}

		// Recent unique workers
		wrows, err := db.pool.Query(ctx, `
			SELECT DISTINCT ON (worker_display)
			       COALESCE(w.name, ph.worker_name_override, 'Unknown') as worker_display
			FROM photos ph
			LEFT JOIN workers w ON ph.worker_id = w.id
			WHERE ph.project_id = $1
			ORDER BY worker_display, ph.uploaded_at DESC
			LIMIT 4
		`, pid)
		if err == nil {
			for wrows.Next() {
				var name string
				wrows.Scan(&name)
				summaries[i].RecentWorkers = append(summaries[i].RecentWorkers, name)
			}
			wrows.Close()
		}
	}

	return summaries, nil
}

func (db *DB) GetProjectBySlug(ctx context.Context, slug string) (*Project, error) {
	var p Project
	err := db.pool.QueryRow(ctx, `
		SELECT id, name, slug, address, description, active, created_at
		FROM projects WHERE slug = $1
	`, slug).Scan(&p.ID, &p.Name, &p.Slug, &p.Address, &p.Description, &p.Active, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (db *DB) GetProjectByID(ctx context.Context, id int) (*Project, error) {
	var p Project
	err := db.pool.QueryRow(ctx, `
		SELECT id, name, slug, address, description, active, created_at
		FROM projects WHERE id = $1
	`, id).Scan(&p.ID, &p.Name, &p.Slug, &p.Address, &p.Description, &p.Active, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (db *DB) CreateProject(ctx context.Context, name, slug string, address, description *string) (*Project, error) {
	var p Project
	err := db.pool.QueryRow(ctx, `
		INSERT INTO projects (name, slug, address, description)
		VALUES ($1, $2, $3, $4)
		RETURNING id, name, slug, address, description, active, created_at
	`, name, slug, address, description).Scan(
		&p.ID, &p.Name, &p.Slug, &p.Address, &p.Description, &p.Active, &p.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (db *DB) ToggleProjectActive(ctx context.Context, id int) (*Project, error) {
	var p Project
	err := db.pool.QueryRow(ctx, `
		UPDATE projects SET active = NOT active WHERE id = $1
		RETURNING id, name, slug, address, description, active, created_at
	`, id).Scan(&p.ID, &p.Name, &p.Slug, &p.Address, &p.Description, &p.Active, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ─── Worker Queries ──────────────────────────────────────────────

func (db *DB) ListActiveWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, name, short_code, active, created_at
		FROM workers WHERE active = true ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workers []Worker
	for rows.Next() {
		var w Worker
		if err := rows.Scan(&w.ID, &w.Name, &w.ShortCode, &w.Active, &w.CreatedAt); err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, nil
}

func (db *DB) CreateWorker(ctx context.Context, name, shortCode string) (*Worker, error) {
	var w Worker
	err := db.pool.QueryRow(ctx, `
		INSERT INTO workers (name, short_code) VALUES ($1, $2)
		RETURNING id, name, short_code, active, created_at
	`, name, shortCode).Scan(&w.ID, &w.Name, &w.ShortCode, &w.Active, &w.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (db *DB) ToggleWorkerActive(ctx context.Context, id int) (*Worker, error) {
	var w Worker
	err := db.pool.QueryRow(ctx, `
		UPDATE workers SET active = NOT active WHERE id = $1
		RETURNING id, name, short_code, active, created_at
	`, id).Scan(&w.ID, &w.Name, &w.ShortCode, &w.Active, &w.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// ─── Photo Queries ───────────────────────────────────────────────

func (db *DB) GetPhotosForProject(ctx context.Context, projectID int) ([]PhotoWithWorker, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT ph.id, ph.project_id, ph.worker_id, ph.worker_name_override,
		       ph.filename, ph.original_filename, ph.caption, ph.tag,
		       ph.lat, ph.lng, ph.taken_at, ph.uploaded_at,
		       ph.file_size, ph.width, ph.height,
		       ph.storage_path, ph.thumb_path, ph.upload_batch,
		       COALESCE(w.name, '') as worker_name
		FROM photos ph
		LEFT JOIN workers w ON ph.worker_id = w.id
		WHERE ph.project_id = $1
		ORDER BY ph.uploaded_at DESC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var photos []PhotoWithWorker
	for rows.Next() {
		var p PhotoWithWorker
		err := rows.Scan(
			&p.ID, &p.ProjectID, &p.WorkerID, &p.WorkerNameOverride,
			&p.Filename, &p.OriginalFilename, &p.Caption, &p.Tag,
			&p.Lat, &p.Lng, &p.TakenAt, &p.UploadedAt,
			&p.FileSize, &p.Width, &p.Height,
			&p.StoragePath, &p.ThumbPath, &p.UploadBatch,
			&p.WorkerName,
		)
		if err != nil {
			return nil, err
		}
		photos = append(photos, p)
	}
	return photos, nil
}

func (db *DB) GetPhotosForProjectPaginated(ctx context.Context, projectID, offset, limit int) ([]PhotoWithWorker, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT ph.id, ph.project_id, ph.worker_id, ph.worker_name_override,
		       ph.filename, ph.original_filename, ph.caption, ph.tag,
		       ph.lat, ph.lng, ph.taken_at, ph.uploaded_at,
		       ph.file_size, ph.width, ph.height,
		       ph.storage_path, ph.thumb_path, ph.upload_batch,
		       COALESCE(w.name, '') as worker_name
		FROM photos ph
		LEFT JOIN workers w ON ph.worker_id = w.id
		WHERE ph.project_id = $1
		ORDER BY ph.uploaded_at DESC
		OFFSET $2 LIMIT $3
	`, projectID, offset, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var photos []PhotoWithWorker
	for rows.Next() {
		var p PhotoWithWorker
		err := rows.Scan(
			&p.ID, &p.ProjectID, &p.WorkerID, &p.WorkerNameOverride,
			&p.Filename, &p.OriginalFilename, &p.Caption, &p.Tag,
			&p.Lat, &p.Lng, &p.TakenAt, &p.UploadedAt,
			&p.FileSize, &p.Width, &p.Height,
			&p.StoragePath, &p.ThumbPath, &p.UploadBatch,
			&p.WorkerName,
		)
		if err != nil {
			return nil, err
		}
		photos = append(photos, p)
	}
	return photos, nil
}

func (db *DB) InsertPhoto(ctx context.Context, p *Photo) error {
	return db.pool.QueryRow(ctx, `
		INSERT INTO photos (
			project_id, worker_id, worker_name_override, filename,
			original_filename, caption, tag, lat, lng, taken_at,
			file_size, width, height, storage_path, thumb_path, upload_batch
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING id, uploaded_at
	`,
		p.ProjectID, p.WorkerID, p.WorkerNameOverride, p.Filename,
		p.OriginalFilename, p.Caption, p.Tag, p.Lat, p.Lng, p.TakenAt,
		p.FileSize, p.Width, p.Height, p.StoragePath, p.ThumbPath, p.UploadBatch,
	).Scan(&p.ID, &p.UploadedAt)
}

func (db *DB) GetPhotoByID(ctx context.Context, id int) (*Photo, error) {
	var p Photo
	err := db.pool.QueryRow(ctx, `
		SELECT id, project_id, worker_id, worker_name_override, filename,
		       original_filename, caption, tag, lat, lng, taken_at, uploaded_at,
		       file_size, width, height, storage_path, thumb_path, upload_batch
		FROM photos WHERE id = $1
	`, id).Scan(
		&p.ID, &p.ProjectID, &p.WorkerID, &p.WorkerNameOverride, &p.Filename,
		&p.OriginalFilename, &p.Caption, &p.Tag, &p.Lat, &p.Lng, &p.TakenAt, &p.UploadedAt,
		&p.FileSize, &p.Width, &p.Height, &p.StoragePath, &p.ThumbPath, &p.UploadBatch,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (db *DB) UpdatePhotoDimensions(ctx context.Context, photoID, width, height int) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE photos SET width = $2, height = $3 WHERE id = $1
	`, photoID, width, height)
	return err
}

func (db *DB) UpdatePhotoAnnotations(ctx context.Context, photoID int, caption, tag *string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE photos SET caption = $2, tag = $3 WHERE id = $1
	`, photoID, caption, tag)
	return err
}

// ─── Admin Queries ───────────────────────────────────────────────

func (db *DB) ListAllProjects(ctx context.Context) ([]AdminProject, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT p.id, p.name, p.slug, p.address, p.description, p.active, p.created_at,
		       COALESCE(s.cnt, 0)
		FROM projects p
		LEFT JOIN (
			SELECT project_id, COUNT(*) as cnt FROM photos GROUP BY project_id
		) s ON p.id = s.project_id
		ORDER BY p.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []AdminProject
	for rows.Next() {
		var ap AdminProject
		err := rows.Scan(
			&ap.ID, &ap.Name, &ap.Slug, &ap.Address, &ap.Description,
			&ap.Active, &ap.CreatedAt, &ap.PhotoCount,
		)
		if err != nil {
			return nil, err
		}
		projects = append(projects, ap)
	}
	return projects, nil
}

func (db *DB) ListAllWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, name, short_code, active, created_at
		FROM workers ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workers []Worker
	for rows.Next() {
		var w Worker
		if err := rows.Scan(&w.ID, &w.Name, &w.ShortCode, &w.Active, &w.CreatedAt); err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, nil
}

func (db *DB) PhotoCountForProject(ctx context.Context, projectID int) int {
	var count int
	db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM photos WHERE project_id = $1`, projectID).Scan(&count)
	return count
}
