package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// ─── Inspection Photo Model ────────────────────────────────────

// InspectionPhoto links a photo to a specific inspection and optionally to a checklist item.
type InspectionPhoto struct {
	ID           int       `json:"id"`
	InspectionID int       `json:"inspection_id"`
	ItemID       *int      `json:"item_id,omitempty"`    // template item ID (null = general inspection photo)
	AdhocItemID  *int      `json:"adhoc_item_id,omitempty"` // adhoc item ID (null = not an adhoc item photo)
	Filename     string    `json:"filename"`
	StoragePath  string    `json:"storage_path"`
	ThumbPath    *string   `json:"thumb_path,omitempty"`
	Caption      *string   `json:"caption,omitempty"`
	Lat          *float64  `json:"lat,omitempty"`
	Lng          *float64  `json:"lng,omitempty"`
	FileSize     *int      `json:"file_size,omitempty"`
	Width        *int      `json:"width,omitempty"`
	Height       *int      `json:"height,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// InspectionListItem is a detailed view for listing inspections with photo counts (API use).
type InspectionListItem struct {
	Inspection  Inspection `json:"inspection"`
	TemplateName *string   `json:"template_name,omitempty"`
	TotalItems  int        `json:"total_items"`
	CompletedItems int     `json:"completed_items"`
	PhotoCount  int        `json:"photo_count"`
	ProgressPct int        `json:"progress_pct"`
}

// ─── Inspection Photos Table ───────────────────────────────────

// CreateInspectionPhotosTables creates the inspection_photos table.
func (db *DB) CreateInspectionPhotosTables(ctx context.Context) error {
	sql := `
	CREATE TABLE IF NOT EXISTS inspection_photos (
		id SERIAL PRIMARY KEY,
		inspection_id INTEGER NOT NULL REFERENCES inspections(id) ON DELETE CASCADE,
		item_id INTEGER REFERENCES inspection_template_items(id) ON DELETE SET NULL,
		adhoc_item_id INTEGER REFERENCES inspection_adhoc_items(id) ON DELETE SET NULL,
		filename VARCHAR(500) NOT NULL,
		storage_path TEXT NOT NULL,
		thumb_path TEXT,
		caption TEXT,
		lat DOUBLE PRECISION,
		lng DOUBLE PRECISION,
		file_size INTEGER,
		width INTEGER,
		height INTEGER,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_insp_photos_inspection ON inspection_photos(inspection_id);
	CREATE INDEX IF NOT EXISTS idx_insp_photos_item ON inspection_photos(item_id);
	CREATE INDEX IF NOT EXISTS idx_insp_photos_adhoc ON inspection_photos(adhoc_item_id);
	`
	_, err := db.pool.Exec(ctx, sql)
	return err
}

// ─── Inspection Photo CRUD ─────────────────────────────────────

// InsertInspectionPhoto adds a photo attachment to an inspection, optionally linked to a checklist item.
func (db *DB) InsertInspectionPhoto(ctx context.Context, p *InspectionPhoto) error {
	err := db.pool.QueryRow(ctx, `
		INSERT INTO inspection_photos (inspection_id, item_id, adhoc_item_id, filename, storage_path,
		                                thumb_path, caption, lat, lng, file_size, width, height)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, created_at
	`, p.InspectionID, p.ItemID, p.AdhocItemID, p.Filename, p.StoragePath,
		p.ThumbPath, p.Caption, p.Lat, p.Lng, p.FileSize, p.Width, p.Height,
	).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert inspection photo: %w", err)
	}
	// Touch inspection updated_at
	db.pool.Exec(ctx, `UPDATE inspections SET updated_at = NOW() WHERE id = $1`, p.InspectionID)
	return nil
}

// GetInspectionPhotos returns all photos for a given inspection, ordered by creation time.
func (db *DB) GetInspectionPhotos(ctx context.Context, inspectionID int) ([]InspectionPhoto, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, inspection_id, item_id, adhoc_item_id, filename, storage_path,
		       thumb_path, caption, lat, lng, file_size, width, height, created_at
		FROM inspection_photos
		WHERE inspection_id = $1
		ORDER BY created_at DESC
	`, inspectionID)
	if err != nil {
		return nil, fmt.Errorf("get inspection photos: %w", err)
	}
	defer rows.Close()

	var photos []InspectionPhoto
	for rows.Next() {
		var p InspectionPhoto
		if err := rows.Scan(
			&p.ID, &p.InspectionID, &p.ItemID, &p.AdhocItemID, &p.Filename, &p.StoragePath,
			&p.ThumbPath, &p.Caption, &p.Lat, &p.Lng, &p.FileSize, &p.Width, &p.Height, &p.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan inspection photo: %w", err)
		}
		photos = append(photos, p)
	}
	return photos, nil
}

// GetInspectionPhotosByItem returns photos linked to a specific template checklist item within an inspection.
func (db *DB) GetInspectionPhotosByItem(ctx context.Context, inspectionID, itemID int) ([]InspectionPhoto, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, inspection_id, item_id, adhoc_item_id, filename, storage_path,
		       thumb_path, caption, lat, lng, file_size, width, height, created_at
		FROM inspection_photos
		WHERE inspection_id = $1 AND item_id = $2
		ORDER BY created_at DESC
	`, inspectionID, itemID)
	if err != nil {
		return nil, fmt.Errorf("get photos by item: %w", err)
	}
	defer rows.Close()

	var photos []InspectionPhoto
	for rows.Next() {
		var p InspectionPhoto
		if err := rows.Scan(
			&p.ID, &p.InspectionID, &p.ItemID, &p.AdhocItemID, &p.Filename, &p.StoragePath,
			&p.ThumbPath, &p.Caption, &p.Lat, &p.Lng, &p.FileSize, &p.Width, &p.Height, &p.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan item photo: %w", err)
		}
		photos = append(photos, p)
	}
	return photos, nil
}

// GetInspectionPhotosByAdhocItem returns photos linked to a specific ad-hoc checklist item.
func (db *DB) GetInspectionPhotosByAdhocItem(ctx context.Context, inspectionID, adhocItemID int) ([]InspectionPhoto, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, inspection_id, item_id, adhoc_item_id, filename, storage_path,
		       thumb_path, caption, lat, lng, file_size, width, height, created_at
		FROM inspection_photos
		WHERE inspection_id = $1 AND adhoc_item_id = $2
		ORDER BY created_at DESC
	`, inspectionID, adhocItemID)
	if err != nil {
		return nil, fmt.Errorf("get photos by adhoc item: %w", err)
	}
	defer rows.Close()

	var photos []InspectionPhoto
	for rows.Next() {
		var p InspectionPhoto
		if err := rows.Scan(
			&p.ID, &p.InspectionID, &p.ItemID, &p.AdhocItemID, &p.Filename, &p.StoragePath,
			&p.ThumbPath, &p.Caption, &p.Lat, &p.Lng, &p.FileSize, &p.Width, &p.Height, &p.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan adhoc photo: %w", err)
		}
		photos = append(photos, p)
	}
	return photos, nil
}

// PhotosByItemResult holds photos grouped by template item ID and adhoc item ID separately,
// avoiding key collisions between template and adhoc items.
type PhotosByItemResult struct {
	ByItemID      map[int][]InspectionPhoto // template item_id -> photos
	ByAdhocItemID map[int][]InspectionPhoto // adhoc_item_id -> photos
}

// GetAllInspectionPhotos returns all photos for an inspection, grouped by item type and ID.
func (db *DB) GetAllInspectionPhotos(ctx context.Context, inspectionID int) (*PhotosByItemResult, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, inspection_id, item_id, adhoc_item_id, filename, storage_path,
		       thumb_path, caption, lat, lng, file_size, width, height, created_at
		FROM inspection_photos
		WHERE inspection_id = $1
		ORDER BY created_at ASC
	`, inspectionID)
	if err != nil {
		return nil, fmt.Errorf("get all inspection photos: %w", err)
	}
	defer rows.Close()

	result := &PhotosByItemResult{
		ByItemID:      make(map[int][]InspectionPhoto),
		ByAdhocItemID: make(map[int][]InspectionPhoto),
	}
	for rows.Next() {
		var p InspectionPhoto
		if err := rows.Scan(
			&p.ID, &p.InspectionID, &p.ItemID, &p.AdhocItemID, &p.Filename, &p.StoragePath,
			&p.ThumbPath, &p.Caption, &p.Lat, &p.Lng, &p.FileSize, &p.Width, &p.Height, &p.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan inspection photo: %w", err)
		}
		if p.AdhocItemID != nil && *p.AdhocItemID > 0 {
			result.ByAdhocItemID[*p.AdhocItemID] = append(result.ByAdhocItemID[*p.AdhocItemID], p)
		} else if p.ItemID != nil {
			result.ByItemID[*p.ItemID] = append(result.ByItemID[*p.ItemID], p)
		}
	}
	return result, nil
}

// GetInspectionPhotoByID returns a single inspection photo by ID.
func (db *DB) GetInspectionPhotoByID(ctx context.Context, photoID int) (*InspectionPhoto, error) {
	var p InspectionPhoto
	err := db.pool.QueryRow(ctx, `
		SELECT id, inspection_id, item_id, adhoc_item_id, filename, storage_path,
		       thumb_path, caption, lat, lng, file_size, width, height, created_at
		FROM inspection_photos WHERE id = $1
	`, photoID).Scan(
		&p.ID, &p.InspectionID, &p.ItemID, &p.AdhocItemID, &p.Filename, &p.StoragePath,
		&p.ThumbPath, &p.Caption, &p.Lat, &p.Lng, &p.FileSize, &p.Width, &p.Height, &p.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get inspection photo: %w", err)
	}
	return &p, nil
}

// DeleteInspectionPhoto removes a photo from an inspection.
func (db *DB) DeleteInspectionPhoto(ctx context.Context, inspectionID, photoID int) error {
	result, err := db.pool.Exec(ctx, `
		DELETE FROM inspection_photos WHERE id = $1 AND inspection_id = $2
	`, photoID, inspectionID)
	if err != nil {
		return fmt.Errorf("delete inspection photo: %w", err)
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("photo %d not found in inspection %d", photoID, inspectionID)
	}
	db.pool.Exec(ctx, `UPDATE inspections SET updated_at = NOW() WHERE id = $1`, inspectionID)
	return nil
}

// CountInspectionPhotos returns the total number of photos for an inspection.
func (db *DB) CountInspectionPhotos(ctx context.Context, inspectionID int) int {
	var count int
	db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM inspection_photos WHERE inspection_id = $1`, inspectionID).Scan(&count)
	return count
}

// CountInspectionPhotosByItem returns the number of photos for a specific checklist item.
func (db *DB) CountInspectionPhotosByItem(ctx context.Context, inspectionID, itemID int) int {
	var count int
	db.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM inspection_photos WHERE inspection_id = $1 AND item_id = $2
	`, inspectionID, itemID).Scan(&count)
	return count
}

// ─── Inspection Listing & Summaries ────────────────────────────

// ListInspections returns all inspections with summary data, ordered by most recent first.
func (db *DB) ListInspections(ctx context.Context) ([]InspectionListItem, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT i.id, i.template_id, i.property_id, i.property_name, i.building_id, i.unit_id,
		       i.inspector_id, i.inspector_name, i.status, i.notes,
		       i.created_at, i.updated_at, i.completed_at,
		       t.name as template_name,
		       COALESCE(r.total, 0) as total_items,
		       COALESCE(r.completed, 0) as completed_items,
		       COALESCE(p.photo_count, 0) as photo_count
		FROM inspections i
		LEFT JOIN inspection_templates t ON i.template_id = t.id
		LEFT JOIN (
			SELECT inspection_id,
			       COUNT(*) as total,
			       COUNT(status) as completed
			FROM inspection_responses
			GROUP BY inspection_id
		) r ON i.id = r.inspection_id
		LEFT JOIN (
			SELECT inspection_id, COUNT(*) as photo_count
			FROM inspection_photos
			GROUP BY inspection_id
		) p ON i.id = p.inspection_id
		ORDER BY i.updated_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list inspections: %w", err)
	}
	defer rows.Close()

	var summaries []InspectionListItem
	for rows.Next() {
		var s InspectionListItem
		if err := rows.Scan(
			&s.Inspection.ID, &s.Inspection.TemplateID, &s.Inspection.PropertyID,
			&s.Inspection.PropertyName, &s.Inspection.BuildingID, &s.Inspection.UnitID,
			&s.Inspection.InspectorID, &s.Inspection.InspectorName,
			&s.Inspection.Status, &s.Inspection.Notes,
			&s.Inspection.CreatedAt, &s.Inspection.UpdatedAt, &s.Inspection.CompletedAt,
			&s.TemplateName, &s.TotalItems, &s.CompletedItems, &s.PhotoCount,
		); err != nil {
			return nil, fmt.Errorf("scan inspection summary: %w", err)
		}
		if s.TotalItems > 0 {
			s.ProgressPct = (s.CompletedItems * 100) / s.TotalItems
		}
		summaries = append(summaries, s)
	}
	return summaries, nil
}

// ListInspectionsByStatus returns inspections filtered by status.
func (db *DB) ListInspectionsByStatus(ctx context.Context, status string) ([]InspectionListItem, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT i.id, i.template_id, i.property_id, i.property_name, i.building_id, i.unit_id,
		       i.inspector_id, i.inspector_name, i.status, i.notes,
		       i.created_at, i.updated_at, i.completed_at,
		       t.name as template_name,
		       COALESCE(r.total, 0) as total_items,
		       COALESCE(r.completed, 0) as completed_items,
		       COALESCE(p.photo_count, 0) as photo_count
		FROM inspections i
		LEFT JOIN inspection_templates t ON i.template_id = t.id
		LEFT JOIN (
			SELECT inspection_id,
			       COUNT(*) as total,
			       COUNT(status) as completed
			FROM inspection_responses
			GROUP BY inspection_id
		) r ON i.id = r.inspection_id
		LEFT JOIN (
			SELECT inspection_id, COUNT(*) as photo_count
			FROM inspection_photos
			GROUP BY inspection_id
		) p ON i.id = p.inspection_id
		WHERE i.status = $1
		ORDER BY i.updated_at DESC
	`, status)
	if err != nil {
		return nil, fmt.Errorf("list inspections by status: %w", err)
	}
	defer rows.Close()

	var summaries []InspectionListItem
	for rows.Next() {
		var s InspectionListItem
		if err := rows.Scan(
			&s.Inspection.ID, &s.Inspection.TemplateID, &s.Inspection.PropertyID,
			&s.Inspection.PropertyName, &s.Inspection.BuildingID, &s.Inspection.UnitID,
			&s.Inspection.InspectorID, &s.Inspection.InspectorName,
			&s.Inspection.Status, &s.Inspection.Notes,
			&s.Inspection.CreatedAt, &s.Inspection.UpdatedAt, &s.Inspection.CompletedAt,
			&s.TemplateName, &s.TotalItems, &s.CompletedItems, &s.PhotoCount,
		); err != nil {
			return nil, fmt.Errorf("scan inspection summary: %w", err)
		}
		if s.TotalItems > 0 {
			s.ProgressPct = (s.CompletedItems * 100) / s.TotalItems
		}
		summaries = append(summaries, s)
	}
	return summaries, nil
}

// ─── Inspection Update & Delete ────────────────────────────────

// UpdateInspectionNotes updates the top-level notes on an inspection.
func (db *DB) UpdateInspectionNotes(ctx context.Context, inspectionID int, notes *string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE inspections SET notes = $1, updated_at = NOW() WHERE id = $2
	`, notes, inspectionID)
	if err != nil {
		return fmt.Errorf("update inspection notes: %w", err)
	}
	return nil
}

// UpdateResponseNotes updates notes on a specific response without changing the status.
func (db *DB) UpdateResponseNotes(ctx context.Context, inspectionID, itemID int, notes *string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE inspection_responses SET notes = $1, updated_at = NOW()
		WHERE inspection_id = $2 AND item_id = $3
	`, notes, inspectionID, itemID)
	if err != nil {
		return fmt.Errorf("update response notes: %w", err)
	}
	db.pool.Exec(ctx, `UPDATE inspections SET updated_at = NOW() WHERE id = $1`, inspectionID)
	return nil
}

// DeleteInspection removes an inspection and all its responses and photos (via CASCADE).
func (db *DB) DeleteInspection(ctx context.Context, inspectionID int) error {
	result, err := db.pool.Exec(ctx, `DELETE FROM inspections WHERE id = $1`, inspectionID)
	if err != nil {
		return fmt.Errorf("delete inspection: %w", err)
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("inspection %d not found", inspectionID)
	}
	return nil
}

// ReopenInspection sets a completed inspection back to in_progress.
func (db *DB) ReopenInspection(ctx context.Context, inspectionID int) (*Inspection, error) {
	var insp Inspection
	err := db.pool.QueryRow(ctx, `
		UPDATE inspections SET status = 'in_progress', completed_at = NULL, updated_at = NOW()
		WHERE id = $1
		RETURNING id, template_id, property_id, property_name, building_id, unit_id,
		          inspector_id, inspector_name, status, notes, created_at, updated_at, completed_at
	`, inspectionID).Scan(
		&insp.ID, &insp.TemplateID, &insp.PropertyID, &insp.PropertyName,
		&insp.BuildingID, &insp.UnitID, &insp.InspectorID, &insp.InspectorName,
		&insp.Status, &insp.Notes, &insp.CreatedAt, &insp.UpdatedAt, &insp.CompletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("reopen inspection: %w", err)
	}
	return &insp, nil
}

// ─── Inspection Photo Counts per Item (batch) ──────────────────

// PhotoCountByItem maps item_id -> photo count for all items in an inspection.
type PhotoCountByItem struct {
	ItemID     int `json:"item_id"`
	PhotoCount int `json:"photo_count"`
}

// GetPhotoCountsByInspection returns photo counts grouped by item_id for an inspection.
// This is used to show photo badges on checklist items without N+1 queries.
func (db *DB) GetPhotoCountsByInspection(ctx context.Context, inspectionID int) (map[int]int, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT COALESCE(item_id, 0), COUNT(*)
		FROM inspection_photos
		WHERE inspection_id = $1
		GROUP BY item_id
	`, inspectionID)
	if err != nil {
		return nil, fmt.Errorf("get photo counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[int]int)
	for rows.Next() {
		var itemID, count int
		if err := rows.Scan(&itemID, &count); err != nil {
			return nil, fmt.Errorf("scan photo count: %w", err)
		}
		counts[itemID] = count
	}
	return counts, nil
}

// GetAdhocPhotoCountsByInspection returns photo counts grouped by adhoc_item_id for an inspection.
func (db *DB) GetAdhocPhotoCountsByInspection(ctx context.Context, inspectionID int) (map[int]int, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT COALESCE(adhoc_item_id, 0), COUNT(*)
		FROM inspection_photos
		WHERE inspection_id = $1 AND adhoc_item_id IS NOT NULL
		GROUP BY adhoc_item_id
	`, inspectionID)
	if err != nil {
		return nil, fmt.Errorf("get adhoc photo counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[int]int)
	for rows.Next() {
		var itemID, count int
		if err := rows.Scan(&itemID, &count); err != nil {
			return nil, fmt.Errorf("scan adhoc photo count: %w", err)
		}
		counts[itemID] = count
	}
	return counts, nil
}

// UpdateInspectionPhotoDimensions updates width/height for a processed inspection photo.
func (db *DB) UpdateInspectionPhotoDimensions(ctx context.Context, photoID, width, height int) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE inspection_photos SET width = $2, height = $3 WHERE id = $1
	`, photoID, width, height)
	return err
}

// ─── Save Completed Inspection (Single Transaction) ─────────────
// SaveCompletedInspection persists an entire inspection — header, checklist
// results, ad-hoc items, and photo attachment records — in one atomic
// database transaction. If any step fails the whole operation is rolled back,
// guaranteeing data integrity for offline-sync and normal submission flows.

// CompletedInspectionSubmission bundles everything needed to persist
// a finished (or in-progress) inspection in a single transaction.
type CompletedInspectionSubmission struct {
	// Header fields
	TemplateID    *int                       `json:"template_id,omitempty"`
	PropertyID    int                        `json:"property_id"`
	PropertyName  string                     `json:"property_name"`
	BuildingID    *int                       `json:"building_id,omitempty"`
	UnitID        *int                       `json:"unit_id,omitempty"`
	InspectorID   *string                    `json:"inspector_id,omitempty"`
	InspectorName string                     `json:"inspector_name"`
	Notes         *string                    `json:"notes,omitempty"`
	Complete      bool                       `json:"complete"` // true → status='completed', false → 'in_progress'

	// Checklist results
	Responses  []InspectionItemSubmission `json:"responses"`
	AdhocItems []AdhocItemSubmission      `json:"adhoc_items,omitempty"`

	// Photo attachment records (already written to disk — this saves the DB rows)
	Photos []InspectionPhotoRecord `json:"photos,omitempty"`
}

// InspectionPhotoRecord is the metadata for a single photo attachment to be
// batch-inserted as part of a completed inspection save. The file should
// already be written to storage before calling SaveCompletedInspection.
type InspectionPhotoRecord struct {
	ItemID      *int     `json:"item_id,omitempty"`       // template item link (nil = general photo)
	AdhocIndex  *int     `json:"adhoc_index,omitempty"`   // index into AdhocItems for linking after insert
	Filename    string   `json:"filename"`
	StoragePath string   `json:"storage_path"`
	ThumbPath   *string  `json:"thumb_path,omitempty"`
	Caption     *string  `json:"caption,omitempty"`
	Lat         *float64 `json:"lat,omitempty"`
	Lng         *float64 `json:"lng,omitempty"`
	FileSize    *int     `json:"file_size,omitempty"`
	Width       *int     `json:"width,omitempty"`
	Height      *int     `json:"height,omitempty"`
}

// SaveCompletedInspectionResult contains the IDs produced by the transaction.
type SaveCompletedInspectionResult struct {
	Inspection *Inspection `json:"inspection"`
	AdhocIDs   []int       `json:"adhoc_ids,omitempty"`  // ordered adhoc item IDs (same order as submission)
	PhotoIDs   []int       `json:"photo_ids,omitempty"`  // ordered photo IDs (same order as submission)
}

// SaveCompletedInspection atomically persists a full inspection in one transaction:
//  1. Insert the inspection header row.
//  2. Pre-populate template responses (unanswered) then upsert submitted statuses.
//  3. Insert ad-hoc checklist items.
//  4. Batch-insert photo attachment records, linking to the correct item/adhoc IDs.
//
// On any error the transaction is rolled back so no partial data is left behind.
func (db *DB) SaveCompletedInspection(ctx context.Context, sub CompletedInspectionSubmission) (*SaveCompletedInspectionResult, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// ── Step 1: Insert inspection header ──────────────────────────
	status := "in_progress"
	if sub.Complete {
		status = "completed"
	}

	var insp Inspection
	err = tx.QueryRow(ctx, `
		INSERT INTO inspections (template_id, property_id, property_name, building_id, unit_id,
		                         inspector_id, inspector_name, status, notes,
		                         completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
		        CASE WHEN $8 = 'completed' THEN NOW() ELSE NULL END)
		RETURNING id, template_id, property_id, property_name, building_id, unit_id,
		          inspector_id, inspector_name, status, notes, created_at, updated_at, completed_at
	`, sub.TemplateID, sub.PropertyID, sub.PropertyName, sub.BuildingID, sub.UnitID,
		sub.InspectorID, sub.InspectorName, status, sub.Notes).Scan(
		&insp.ID, &insp.TemplateID, &insp.PropertyID, &insp.PropertyName,
		&insp.BuildingID, &insp.UnitID, &insp.InspectorID, &insp.InspectorName,
		&insp.Status, &insp.Notes, &insp.CreatedAt, &insp.UpdatedAt, &insp.CompletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert inspection header: %w", err)
	}

	// ── Step 2: Batch-insert checklist results ────────────────────

	// Pre-populate all template items as unanswered (status = NULL) if template-based
	if sub.TemplateID != nil && *sub.TemplateID > 0 {
		_, err = tx.Exec(ctx, `
			INSERT INTO inspection_responses (inspection_id, item_id)
			SELECT $1, i.id
			FROM inspection_template_items i
			JOIN inspection_template_categories c ON i.category_id = c.id
			WHERE c.template_id = $2
		`, insp.ID, *sub.TemplateID)
		if err != nil {
			return nil, fmt.Errorf("pre-populate template responses: %w", err)
		}
	}

	// Upsert each submitted response (sets actual pass/fail/needs_attention status)
	for _, resp := range sub.Responses {
		if !ValidItemStatuses[resp.Status] {
			log.Printf("SaveCompletedInspection: skipping invalid status %q for item %d", resp.Status, resp.ItemID)
			continue
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO inspection_responses (inspection_id, item_id, status, notes, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (inspection_id, item_id)
			DO UPDATE SET status = $3, notes = $4, updated_at = NOW()
		`, insp.ID, resp.ItemID, resp.Status, resp.Notes)
		if err != nil {
			return nil, fmt.Errorf("upsert response for item %d: %w", resp.ItemID, err)
		}
	}

	// ── Step 3: Insert ad-hoc items ───────────────────────────────
	adhocIDs := make([]int, 0, len(sub.AdhocItems))
	for i, adhoc := range sub.AdhocItems {
		catName := adhoc.CategoryName
		if catName == "" {
			catName = "Ad-hoc Items"
		}
		var statusStr *string
		if adhoc.Status != nil {
			s := string(*adhoc.Status)
			statusStr = &s
		}
		var adhocID int
		err = tx.QueryRow(ctx, `
			INSERT INTO inspection_adhoc_items (inspection_id, category_name, label, description, status, notes, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id
		`, insp.ID, catName, adhoc.Label, adhoc.Description, statusStr, adhoc.Notes, i+1).Scan(&adhocID)
		if err != nil {
			return nil, fmt.Errorf("insert adhoc item %q: %w", adhoc.Label, err)
		}
		adhocIDs = append(adhocIDs, adhocID)
	}

	// ── Step 4: Batch-insert photo attachment records ─────────────
	photoIDs := make([]int, 0, len(sub.Photos))
	for _, photo := range sub.Photos {
		// Resolve the item linkage
		var itemID *int
		var adhocItemID *int

		if photo.ItemID != nil && *photo.ItemID > 0 {
			// Link to a template checklist item
			itemID = photo.ItemID
		} else if photo.AdhocIndex != nil && *photo.AdhocIndex >= 0 && *photo.AdhocIndex < len(adhocIDs) {
			// Link to an ad-hoc item using the index → ID mapping from step 3
			aid := adhocIDs[*photo.AdhocIndex]
			adhocItemID = &aid
		}

		var photoID int
		err = tx.QueryRow(ctx, `
			INSERT INTO inspection_photos (inspection_id, item_id, adhoc_item_id, filename, storage_path,
			                                thumb_path, caption, lat, lng, file_size, width, height)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING id
		`, insp.ID, itemID, adhocItemID, photo.Filename, photo.StoragePath,
			photo.ThumbPath, photo.Caption, photo.Lat, photo.Lng, photo.FileSize, photo.Width, photo.Height,
		).Scan(&photoID)
		if err != nil {
			return nil, fmt.Errorf("insert photo %q: %w", photo.Filename, err)
		}
		photoIDs = append(photoIDs, photoID)
	}

	// ── Commit ────────────────────────────────────────────────────
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	return &SaveCompletedInspectionResult{
		Inspection: &insp,
		AdhocIDs:   adhocIDs,
		PhotoIDs:   photoIDs,
	}, nil
}
