package main

import (
	"context"
	"fmt"
	"log"
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
	// PropertyOS linkage (claudecode-u61f). All nullable — worker uploads leave
	// these unset; PropertyOS uploads fill them. Keyed durably on UnitID; the
	// tenant fields are a point-in-time stamp. Scope = "property" | "tenant".
	BuildingID *int
	UnitID     *int
	TenantID   *int
	TenantName *string
	Scope      *string
}

// PhotoWithWorker includes the resolved worker name from a JOIN.
type PhotoWithWorker struct {
	Photo
	WorkerName  string
	ProjectSlug string // set by building-scoped reads; empty otherwise
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

	-- PropertyOS linkage (claudecode-u61f): photos uploaded from the PropertyOS
	-- building report carry the originating building + (optionally) the suite/unit
	-- and the tenant that occupied it at upload time. Keyed durably on unit_id;
	-- tenant_id/tenant_name are stamped for context and survive turnover.
	-- scope = 'property' (building-wide) | 'tenant' (a specific suite/tenant).
	ALTER TABLE photos ADD COLUMN IF NOT EXISTS building_id  INTEGER;
	ALTER TABLE photos ADD COLUMN IF NOT EXISTS unit_id      INTEGER;
	ALTER TABLE photos ADD COLUMN IF NOT EXISTS tenant_id    INTEGER;
	ALTER TABLE photos ADD COLUMN IF NOT EXISTS tenant_name  VARCHAR(300);
	ALTER TABLE photos ADD COLUMN IF NOT EXISTS scope        VARCHAR(20);
	CREATE INDEX IF NOT EXISTS idx_photos_building_id     ON photos(building_id);
	CREATE INDEX IF NOT EXISTS idx_photos_building_unit   ON photos(building_id, unit_id);
	CREATE INDEX IF NOT EXISTS idx_photos_building_tenant ON photos(building_id, tenant_id);
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

	// Enrich each summary with its recent photos + workers. Previously this ran
	// two queries PER project (O(2N+1) round-trips on the home page); now it is
	// two windowed queries total, fanned back out to the summaries in O(1) via
	// an index keyed on project id. Enrichment failures degrade gracefully — a
	// missing recent-photos/workers set still renders the project card.
	byID := make(map[int]*ProjectSummary, len(summaries))
	for i := range summaries {
		byID[summaries[i].Project.ID] = &summaries[i]
	}

	// Recent 4 photos per active project, ranked newest-first. The id tiebreaker
	// makes the cut deterministic when uploaded_at ties.
	if prows, err := db.pool.Query(ctx, `
		SELECT project_id, id, filename, uploaded_at FROM (
			SELECT ph.project_id, ph.id, ph.filename, ph.uploaded_at,
			       ROW_NUMBER() OVER (PARTITION BY ph.project_id
			                          ORDER BY ph.uploaded_at DESC, ph.id DESC) AS rn
			FROM photos ph
			JOIN projects p ON p.id = ph.project_id AND p.active = true
		) t
		WHERE rn <= 4
		ORDER BY project_id, rn
	`); err != nil {
		log.Printf("ListProjectSummaries: recent-photos query: %v", err)
	} else {
		for prows.Next() {
			var pid int
			var p Photo
			if err := prows.Scan(&pid, &p.ID, &p.Filename, &p.UploadedAt); err != nil {
				log.Printf("ListProjectSummaries: recent-photo scan: %v", err)
				continue
			}
			if ps := byID[pid]; ps != nil {
				ps.RecentPhotos = append(ps.RecentPhotos, p)
			}
		}
		if err := prows.Err(); err != nil {
			log.Printf("ListProjectSummaries: recent-photo rows: %v", err)
		}
		prows.Close()
	}

	// Recent up-to-4 distinct workers per active project. Preserves the prior
	// shape: dedup worker names, then take the first 4 alphabetically.
	if wrows, err := db.pool.Query(ctx, `
		SELECT project_id, worker_display FROM (
			SELECT d.project_id, d.worker_display,
			       ROW_NUMBER() OVER (PARTITION BY d.project_id
			                          ORDER BY d.worker_display) AS rn
			FROM (
				SELECT ph.project_id,
				       COALESCE(w.name, ph.worker_name_override, 'Unknown') AS worker_display
				FROM photos ph
				LEFT JOIN workers w ON ph.worker_id = w.id
				JOIN projects p ON p.id = ph.project_id AND p.active = true
				GROUP BY ph.project_id, COALESCE(w.name, ph.worker_name_override, 'Unknown')
			) d
		) t
		WHERE rn <= 4
		ORDER BY project_id, rn
	`); err != nil {
		log.Printf("ListProjectSummaries: recent-workers query: %v", err)
	} else {
		for wrows.Next() {
			var pid int
			var name string
			if err := wrows.Scan(&pid, &name); err != nil {
				log.Printf("ListProjectSummaries: recent-worker scan: %v", err)
				continue
			}
			if ps := byID[pid]; ps != nil {
				ps.RecentWorkers = append(ps.RecentWorkers, name)
			}
		}
		if err := wrows.Err(); err != nil {
			log.Printf("ListProjectSummaries: recent-worker rows: %v", err)
		}
		wrows.Close()
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

// GetOrCreateProjectForBuilding resolves the DukeCam project that backs a
// PropertyOS building, creating it on first use so the "no project linked"
// banner clears itself (claudecode-u61f). Resolution order:
//  1. preferredSlug (the buildings.dukecam_slug PropertyOS already knows), if set
//  2. a slug derived from the building name
//  3. otherwise create a new project from name/address
//
// Returns the project so the caller can hand the slug back to PropertyOS to
// persist. Concurrency-safe: a lost insert race falls back to a re-fetch.
func (db *DB) GetOrCreateProjectForBuilding(ctx context.Context, preferredSlug, name string, address *string) (*Project, error) {
	if preferredSlug != "" {
		if p, err := db.GetProjectBySlug(ctx, preferredSlug); err == nil {
			return p, nil
		}
	}
	slug := slugify(name)
	if slug == "" {
		return nil, fmt.Errorf("cannot derive slug from building name %q", name)
	}
	if p, err := db.GetProjectBySlug(ctx, slug); err == nil {
		return p, nil
	}
	p, err := db.CreateProject(ctx, name, slug, address, nil)
	if err != nil {
		// Likely a unique-slug race with a concurrent first upload — re-fetch.
		if p2, err2 := db.GetProjectBySlug(ctx, slug); err2 == nil {
			return p2, nil
		}
		return nil, err
	}
	return p, nil
}

// GetPhotosForBuilding returns photos for a building, optionally narrowed to a
// unit or tenant, for the PropertyOS in-app grid (claudecode-u61f). Pass
// unitID/tenantID as nil to skip that filter. Only photos carrying the
// building_id linkage are returned (worker-only uploads are excluded).
func (db *DB) GetPhotosForBuilding(ctx context.Context, buildingID int, unitID, tenantID *int, limit int) ([]PhotoWithWorker, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.pool.Query(ctx, `
		SELECT ph.id, ph.project_id, ph.worker_id, ph.worker_name_override,
		       ph.filename, ph.original_filename, ph.caption, ph.tag,
		       ph.lat, ph.lng, ph.taken_at, ph.uploaded_at,
		       ph.file_size, ph.width, ph.height,
		       ph.storage_path, ph.thumb_path, ph.upload_batch,
		       ph.building_id, ph.unit_id, ph.tenant_id, ph.tenant_name, ph.scope,
		       COALESCE(w.name, '') as worker_name, pr.slug as project_slug
		FROM photos ph
		LEFT JOIN workers w ON ph.worker_id = w.id
		JOIN projects pr ON ph.project_id = pr.id
		WHERE ph.building_id = $1
		  AND ($2::int IS NULL OR ph.unit_id = $2)
		  AND ($3::int IS NULL OR ph.tenant_id = $3)
		ORDER BY ph.uploaded_at DESC
		LIMIT $4
	`, buildingID, unitID, tenantID, limit)
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
			&p.BuildingID, &p.UnitID, &p.TenantID, &p.TenantName, &p.Scope,
			&p.WorkerName, &p.ProjectSlug,
		)
		if err != nil {
			return nil, err
		}
		photos = append(photos, p)
	}
	return photos, nil
}

// InspectionPhotoForBuilding is a slim view of an inspection photo joined to its
// inspection, for surfacing inspection photos in the PropertyOS report Photos
// tab (claudecode follow-up). Inspection photos live in inspection_photos keyed
// by inspection_id; the building link comes from inspections.building_id.
type InspectionPhotoForBuilding struct {
	ID            int
	InspectionID  int
	Caption       *string
	CreatedAt     time.Time
	UnitID        *int
	InspectorName *string
}

// GetInspectionPhotosForBuilding returns photos from COMPLETED inspections tied
// to a building (optionally narrowed to a unit). These are served by id via
// /api/inspections/photos/:id, unlike worker/PropertyOS uploads which live in
// the photos table and are served by slug+filename.
func (db *DB) GetInspectionPhotosForBuilding(ctx context.Context, buildingID int, unitID *int, limit int) ([]InspectionPhotoForBuilding, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.pool.Query(ctx, `
		SELECT ip.id, ip.inspection_id, ip.caption, ip.created_at,
		       i.unit_id, i.inspector_name
		FROM inspection_photos ip
		JOIN inspections i ON i.id = ip.inspection_id
		WHERE i.building_id = $1
		  AND i.status = 'completed'
		  AND ($2::int IS NULL OR i.unit_id = $2)
		ORDER BY ip.created_at DESC
		LIMIT $3
	`, buildingID, unitID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var photos []InspectionPhotoForBuilding
	for rows.Next() {
		var p InspectionPhotoForBuilding
		if err := rows.Scan(&p.ID, &p.InspectionID, &p.Caption, &p.CreatedAt,
			&p.UnitID, &p.InspectorName); err != nil {
			return nil, err
		}
		photos = append(photos, p)
	}
	return photos, nil
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

func (db *DB) FindWorkerByName(ctx context.Context, name string) (*Worker, error) {
	var w Worker
	err := db.pool.QueryRow(ctx, `
		SELECT id, name, short_code, active, created_at
		FROM workers WHERE LOWER(name) = LOWER($1) LIMIT 1
	`, name).Scan(&w.ID, &w.Name, &w.ShortCode, &w.Active, &w.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &w, nil
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
		       ph.scope, ph.tenant_name,
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
			&p.Scope, &p.TenantName,
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
		       ph.scope, ph.tenant_name,
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
			&p.Scope, &p.TenantName,
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
			file_size, width, height, storage_path, thumb_path, upload_batch,
			building_id, unit_id, tenant_id, tenant_name, scope
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		RETURNING id, uploaded_at
	`,
		p.ProjectID, p.WorkerID, p.WorkerNameOverride, p.Filename,
		p.OriginalFilename, p.Caption, p.Tag, p.Lat, p.Lng, p.TakenAt,
		p.FileSize, p.Width, p.Height, p.StoragePath, p.ThumbPath, p.UploadBatch,
		p.BuildingID, p.UnitID, p.TenantID, p.TenantName, p.Scope,
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
