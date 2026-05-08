package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// ─── Inspection Runtime Models ──────────────────────────────────

// ItemStatus represents the three possible states for a checklist item.
type ItemStatus string

const (
	ItemStatusPass           ItemStatus = "pass"
	ItemStatusFail           ItemStatus = "fail"
	ItemStatusNeedsAttention ItemStatus = "needs_attention"
)

// ValidItemStatuses lists acceptable values for validation.
var ValidItemStatuses = map[ItemStatus]bool{
	ItemStatusPass:           true,
	ItemStatusFail:           true,
	ItemStatusNeedsAttention: true,
}

// Inspection is a live or completed inspection record.
type Inspection struct {
	ID             int        `json:"id"`
	TemplateID     *int       `json:"template_id,omitempty"`
	PropertyID     int        `json:"property_id"`
	PropertyName   string     `json:"property_name"`
	BuildingID     *int       `json:"building_id,omitempty"`
	UnitID         *int       `json:"unit_id,omitempty"`
	InspectorID    *string    `json:"inspector_id,omitempty"`
	InspectorName  string     `json:"inspector_name"`
	Status         string     `json:"status"` // draft, in_progress, completed
	Notes          *string    `json:"notes,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// InspectionResponse is a single checklist item's answer within an inspection.
type InspectionResponse struct {
	ID           int        `json:"id"`
	InspectionID int        `json:"inspection_id"`
	ItemID       int        `json:"item_id"`     // references inspection_template_items.id
	Status       *ItemStatus `json:"status"`      // pass, fail, needs_attention, or null (unanswered)
	Notes        *string    `json:"notes,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// InspectionChecklistItem is a checklist item enriched with its current response (for rendering).
type InspectionChecklistItem struct {
	// From template
	ItemID       int     `json:"item_id"`
	Label        string  `json:"label"`
	Description  *string `json:"description,omitempty"`
	RequirePhoto bool    `json:"require_photo"`
	SortOrder    int     `json:"sort_order"`
	// From response (if any)
	ResponseID   *int        `json:"response_id,omitempty"`
	Status       *ItemStatus `json:"status"`
	ResponseNotes *string    `json:"response_notes,omitempty"`
	// Ad-hoc flag (true if this item was added during the inspection, not from the template)
	IsAdhoc      bool    `json:"is_adhoc,omitempty"`
}

// InspectionChecklistCategory groups checklist items with their responses.
type InspectionChecklistCategory struct {
	CategoryID int                       `json:"category_id"`
	Name       string                    `json:"name"`
	SortOrder  int                       `json:"sort_order"`
	Items      []InspectionChecklistItem `json:"items"`
}

// InspectionChecklist is the full inspection with merged template + responses.
type InspectionChecklist struct {
	Inspection Inspection                    `json:"inspection"`
	Categories []InspectionChecklistCategory `json:"categories"`
	Stats      InspectionStats               `json:"stats"`
}

// InspectionStats tracks completion progress.
type InspectionStats struct {
	Total          int `json:"total"`
	Completed      int `json:"completed"`
	Passed         int `json:"passed"`
	Failed         int `json:"failed"`
	NeedsAttention int `json:"needs_attention"`
	ProgressPct    int `json:"progress_pct"`
}

// ─── Runtime Table Creation ─────────────────────────────────────

func (db *DB) CreateInspectionRuntimeTables(ctx context.Context) error {
	sql := `
	CREATE TABLE IF NOT EXISTS inspections (
		id SERIAL PRIMARY KEY,
		template_id INTEGER REFERENCES inspection_templates(id) ON DELETE SET NULL,
		property_id INTEGER NOT NULL DEFAULT 0,
		property_name VARCHAR(500) NOT NULL DEFAULT '',
		building_id INTEGER,
		unit_id INTEGER,
		inspector_id VARCHAR(200),
		inspector_name VARCHAR(300) NOT NULL DEFAULT '',
		status VARCHAR(50) NOT NULL DEFAULT 'draft',
		notes TEXT,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW(),
		completed_at TIMESTAMPTZ
	);
	CREATE INDEX IF NOT EXISTS idx_inspections_status ON inspections(status);

	CREATE TABLE IF NOT EXISTS inspection_responses (
		id SERIAL PRIMARY KEY,
		inspection_id INTEGER NOT NULL REFERENCES inspections(id) ON DELETE CASCADE,
		item_id INTEGER NOT NULL REFERENCES inspection_template_items(id) ON DELETE CASCADE,
		status VARCHAR(50),
		notes TEXT,
		updated_at TIMESTAMPTZ DEFAULT NOW(),
		UNIQUE(inspection_id, item_id)
	);
	CREATE INDEX IF NOT EXISTS idx_insp_resp_inspection ON inspection_responses(inspection_id);
	`
	_, err := db.pool.Exec(ctx, sql)
	return err
}

// ─── Create Inspection ──────────────────────────────────────────

// CreateInspection creates a new inspection and pre-populates responses for all template items.
func (db *DB) CreateInspection(ctx context.Context, templateID *int, propertyID int, propertyName string,
	buildingID, unitID *int, inspectorID *string, inspectorName string, notes *string) (*Inspection, error) {

	var insp Inspection
	err := db.pool.QueryRow(ctx, `
		INSERT INTO inspections (template_id, property_id, property_name, building_id, unit_id,
		                         inspector_id, inspector_name, status, notes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'in_progress', $8)
		RETURNING id, template_id, property_id, property_name, building_id, unit_id,
		          inspector_id, inspector_name, status, notes, created_at, updated_at, completed_at
	`, templateID, propertyID, propertyName, buildingID, unitID, inspectorID, inspectorName, notes).Scan(
		&insp.ID, &insp.TemplateID, &insp.PropertyID, &insp.PropertyName,
		&insp.BuildingID, &insp.UnitID, &insp.InspectorID, &insp.InspectorName,
		&insp.Status, &insp.Notes, &insp.CreatedAt, &insp.UpdatedAt, &insp.CompletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create inspection: %w", err)
	}

	// Pre-populate responses for all items in the template (status = NULL = unanswered)
	if templateID != nil && *templateID > 0 {
		_, err = db.pool.Exec(ctx, `
			INSERT INTO inspection_responses (inspection_id, item_id)
			SELECT $1, i.id
			FROM inspection_template_items i
			JOIN inspection_template_categories c ON i.category_id = c.id
			WHERE c.template_id = $2
		`, insp.ID, *templateID)
		if err != nil {
			return nil, fmt.Errorf("pre-populate responses: %w", err)
		}
	}

	return &insp, nil
}

// ─── Get Inspection by ID ───────────────────────────────────────

func (db *DB) GetInspectionByID(ctx context.Context, id int) (*Inspection, error) {
	var insp Inspection
	err := db.pool.QueryRow(ctx, `
		SELECT id, template_id, property_id, property_name, building_id, unit_id,
		       inspector_id, inspector_name, status, notes, created_at, updated_at, completed_at
		FROM inspections WHERE id = $1
	`, id).Scan(
		&insp.ID, &insp.TemplateID, &insp.PropertyID, &insp.PropertyName,
		&insp.BuildingID, &insp.UnitID, &insp.InspectorID, &insp.InspectorName,
		&insp.Status, &insp.Notes, &insp.CreatedAt, &insp.UpdatedAt, &insp.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &insp, nil
}

// ─── Get Full Checklist (template items merged with responses) ──

func (db *DB) GetInspectionChecklist(ctx context.Context, inspectionID int) (*InspectionChecklist, error) {
	insp, err := db.GetInspectionByID(ctx, inspectionID)
	if err != nil {
		return nil, fmt.Errorf("inspection not found: %w", err)
	}

	if insp.TemplateID == nil || *insp.TemplateID == 0 {
		// Blank inspection — no template checklist, but may have adhoc items
		adhocCats, _ := db.GetAdhocItems(ctx, inspectionID)
		var stats InspectionStats
		for _, ac := range adhocCats {
			for _, item := range ac.Items {
				stats.Total++
				if item.Status != nil {
					stats.Completed++
					switch *item.Status {
					case ItemStatusPass:
						stats.Passed++
					case ItemStatusFail:
						stats.Failed++
					case ItemStatusNeedsAttention:
						stats.NeedsAttention++
					}
				}
			}
		}
		if stats.Total > 0 {
			stats.ProgressPct = (stats.Completed * 100) / stats.Total
		}
		return &InspectionChecklist{
			Inspection: *insp,
			Categories: adhocCats,
			Stats:      stats,
		}, nil
	}

	// Query categories + items + responses in one shot
	rows, err := db.pool.Query(ctx, `
		SELECT c.id, c.name, c.sort_order,
		       i.id, i.label, i.description, i.require_photo, i.sort_order,
		       r.id, r.status, r.notes
		FROM inspection_template_categories c
		JOIN inspection_template_items i ON i.category_id = c.id
		LEFT JOIN inspection_responses r ON r.item_id = i.id AND r.inspection_id = $1
		WHERE c.template_id = $2
		ORDER BY c.sort_order, c.id, i.sort_order, i.id
	`, inspectionID, *insp.TemplateID)
	if err != nil {
		return nil, fmt.Errorf("load checklist: %w", err)
	}
	defer rows.Close()

	catMap := make(map[int]int) // category ID -> index
	var categories []InspectionChecklistCategory
	var stats InspectionStats

	for rows.Next() {
		var (
			catID, catSort                int
			catName                       string
			itemID, itemSort              int
			itemLabel                     string
			itemDesc                      *string
			itemPhoto                     bool
			respID                        *int
			respStatus                    *ItemStatus
			respNotes                     *string
		)
		if err := rows.Scan(
			&catID, &catName, &catSort,
			&itemID, &itemLabel, &itemDesc, &itemPhoto, &itemSort,
			&respID, &respStatus, &respNotes,
		); err != nil {
			return nil, fmt.Errorf("scan checklist row: %w", err)
		}

		// Ensure category exists in map
		idx, exists := catMap[catID]
		if !exists {
			idx = len(categories)
			catMap[catID] = idx
			categories = append(categories, InspectionChecklistCategory{
				CategoryID: catID,
				Name:       catName,
				SortOrder:  catSort,
				Items:      []InspectionChecklistItem{},
			})
		}

		item := InspectionChecklistItem{
			ItemID:        itemID,
			Label:         itemLabel,
			Description:   itemDesc,
			RequirePhoto:  itemPhoto,
			SortOrder:     itemSort,
			ResponseID:    respID,
			Status:        respStatus,
			ResponseNotes: respNotes,
		}
		categories[idx].Items = append(categories[idx].Items, item)

		// Stats
		stats.Total++
		if respStatus != nil {
			stats.Completed++
			switch *respStatus {
			case ItemStatusPass:
				stats.Passed++
			case ItemStatusFail:
				stats.Failed++
			case ItemStatusNeedsAttention:
				stats.NeedsAttention++
			}
		}
	}

	if stats.Total > 0 {
		stats.ProgressPct = (stats.Completed * 100) / stats.Total
	}

	// Append ad-hoc items as additional categories
	adhocCats, err := db.GetAdhocItems(ctx, inspectionID)
	if err != nil {
		log.Printf("load adhoc items warning: %v", err)
		// Non-fatal — continue without adhoc items
	} else {
		for _, ac := range adhocCats {
			categories = append(categories, ac)
			for _, item := range ac.Items {
				stats.Total++
				if item.Status != nil {
					stats.Completed++
					switch *item.Status {
					case ItemStatusPass:
						stats.Passed++
					case ItemStatusFail:
						stats.Failed++
					case ItemStatusNeedsAttention:
						stats.NeedsAttention++
					}
				}
			}
		}
		if stats.Total > 0 {
			stats.ProgressPct = (stats.Completed * 100) / stats.Total
		}
	}

	return &InspectionChecklist{
		Inspection: *insp,
		Categories: categories,
		Stats:      stats,
	}, nil
}

// ─── Update Item Status ─────────────────────────────────────────

// UpdateInspectionItemStatus sets the pass/fail/needs-attention status for a single checklist item.
// Uses UPSERT to handle both new and existing responses.
func (db *DB) UpdateInspectionItemStatus(ctx context.Context, inspectionID, itemID int, status ItemStatus, notes *string) (*InspectionResponse, error) {
	var resp InspectionResponse
	err := db.pool.QueryRow(ctx, `
		INSERT INTO inspection_responses (inspection_id, item_id, status, notes, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (inspection_id, item_id)
		DO UPDATE SET status = $3, notes = $4, updated_at = NOW()
		RETURNING id, inspection_id, item_id, status, notes, updated_at
	`, inspectionID, itemID, status, notes).Scan(
		&resp.ID, &resp.InspectionID, &resp.ItemID, &resp.Status, &resp.Notes, &resp.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("update item status: %w", err)
	}

	// Touch inspection updated_at and auto-advance status
	db.pool.Exec(ctx, `UPDATE inspections SET updated_at = NOW(), status = 'in_progress' WHERE id = $1 AND status = 'draft'`, inspectionID)

	return &resp, nil
}

// ClearInspectionItemStatus removes the status from a checklist item (sets back to unanswered).
func (db *DB) ClearInspectionItemStatus(ctx context.Context, inspectionID, itemID int) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE inspection_responses SET status = NULL, notes = NULL, updated_at = NOW()
		WHERE inspection_id = $1 AND item_id = $2
	`, inspectionID, itemID)
	if err != nil {
		return fmt.Errorf("clear item status: %w", err)
	}
	db.pool.Exec(ctx, `UPDATE inspections SET updated_at = NOW() WHERE id = $1`, inspectionID)
	return nil
}

// CompleteInspection marks an inspection as completed.
func (db *DB) CompleteInspection(ctx context.Context, inspectionID int) (*Inspection, error) {
	var insp Inspection
	err := db.pool.QueryRow(ctx, `
		UPDATE inspections SET status = 'completed', completed_at = NOW(), updated_at = NOW()
		WHERE id = $1
		RETURNING id, template_id, property_id, property_name, building_id, unit_id,
		          inspector_id, inspector_name, status, notes, created_at, updated_at, completed_at
	`, inspectionID).Scan(
		&insp.ID, &insp.TemplateID, &insp.PropertyID, &insp.PropertyName,
		&insp.BuildingID, &insp.UnitID, &insp.InspectorID, &insp.InspectorName,
		&insp.Status, &insp.Notes, &insp.CreatedAt, &insp.UpdatedAt, &insp.CompletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("complete inspection: %w", err)
	}
	return &insp, nil
}

// ─── Bulk Submit Inspection ──────────────────────────────────────

// InspectionSubmission is the payload for submitting a complete inspection at once.
// Used by the offline sync mechanism to submit queued inspections from IndexedDB.
type InspectionSubmission struct {
	TemplateID    *int                       `json:"template_id,omitempty"`
	PropertyID    int                        `json:"property_id"`
	PropertyName  string                     `json:"property_name"`
	BuildingID    *int                       `json:"building_id,omitempty"`
	UnitID        *int                       `json:"unit_id,omitempty"`
	InspectorID   *string                    `json:"inspector_id,omitempty"`
	InspectorName string                     `json:"inspector_name"`
	Notes         *string                    `json:"notes,omitempty"`
	Responses     []InspectionItemSubmission `json:"responses"`
	AdhocItems    []AdhocItemSubmission      `json:"adhoc_items,omitempty"`
	Complete      bool                       `json:"complete"` // if true, mark completed immediately
}

// InspectionItemSubmission is a single checklist item response within a bulk submission.
type InspectionItemSubmission struct {
	ItemID int        `json:"item_id"`
	Status ItemStatus `json:"status"`
	Notes  *string    `json:"notes,omitempty"`
}

// AdhocItemSubmission is an ad-hoc item included in a bulk submission.
type AdhocItemSubmission struct {
	Label        string      `json:"label"`
	CategoryName string      `json:"category_name,omitempty"`
	Description  *string     `json:"description,omitempty"`
	Status       *ItemStatus `json:"status,omitempty"`
	Notes        *string     `json:"notes,omitempty"`
}

// SubmitInspection creates a new inspection and saves all responses in a single transaction.
// This is the primary endpoint for offline sync — an entire inspection is submitted at once.
func (db *DB) SubmitInspection(ctx context.Context, sub InspectionSubmission) (*Inspection, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Determine initial status
	status := "in_progress"
	if sub.Complete {
		status = "completed"
	}

	// Insert the inspection record
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
		return nil, fmt.Errorf("insert inspection: %w", err)
	}

	// Pre-populate all template items as unanswered (if template-based)
	if sub.TemplateID != nil && *sub.TemplateID > 0 {
		_, err = tx.Exec(ctx, `
			INSERT INTO inspection_responses (inspection_id, item_id)
			SELECT $1, i.id
			FROM inspection_template_items i
			JOIN inspection_template_categories c ON i.category_id = c.id
			WHERE c.template_id = $2
		`, insp.ID, *sub.TemplateID)
		if err != nil {
			return nil, fmt.Errorf("pre-populate responses: %w", err)
		}
	}

	// Apply submitted template-item responses (upsert each one)
	for _, resp := range sub.Responses {
		if !ValidItemStatuses[resp.Status] {
			log.Printf("skipping invalid status %q for item %d", resp.Status, resp.ItemID)
			continue
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO inspection_responses (inspection_id, item_id, status, notes, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (inspection_id, item_id)
			DO UPDATE SET status = $3, notes = $4, updated_at = NOW()
		`, insp.ID, resp.ItemID, resp.Status, resp.Notes)
		if err != nil {
			return nil, fmt.Errorf("save response for item %d: %w", resp.ItemID, err)
		}
	}

	// Insert ad-hoc items (if any)
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
		_, err = tx.Exec(ctx, `
			INSERT INTO inspection_adhoc_items (inspection_id, category_name, label, description, status, notes, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, insp.ID, catName, adhoc.Label, adhoc.Description, statusStr, adhoc.Notes, i+1)
		if err != nil {
			return nil, fmt.Errorf("save adhoc item %q: %w", adhoc.Label, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	return &insp, nil
}

// GetInspectionStats returns completion stats for a given inspection (template items + adhoc items).
func (db *DB) GetInspectionStats(ctx context.Context, inspectionID int) (InspectionStats, error) {
	var stats InspectionStats
	// Count template-based responses
	var tTotal, tCompleted, tPassed, tFailed, tNA int
	err := db.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) as total,
			COUNT(r.status) as completed,
			COUNT(*) FILTER (WHERE r.status = 'pass') as passed,
			COUNT(*) FILTER (WHERE r.status = 'fail') as failed,
			COUNT(*) FILTER (WHERE r.status = 'needs_attention') as needs_attention
		FROM inspection_responses r
		WHERE r.inspection_id = $1
	`, inspectionID).Scan(&tTotal, &tCompleted, &tPassed, &tFailed, &tNA)
	if err != nil {
		log.Printf("inspection stats error: %v", err)
		return stats, err
	}

	// Count adhoc items
	var aTotal, aCompleted, aPassed, aFailed, aNA int
	_ = db.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) as total,
			COUNT(status) as completed,
			COUNT(*) FILTER (WHERE status = 'pass') as passed,
			COUNT(*) FILTER (WHERE status = 'fail') as failed,
			COUNT(*) FILTER (WHERE status = 'needs_attention') as needs_attention
		FROM inspection_adhoc_items
		WHERE inspection_id = $1
	`, inspectionID).Scan(&aTotal, &aCompleted, &aPassed, &aFailed, &aNA)

	stats.Total = tTotal + aTotal
	stats.Completed = tCompleted + aCompleted
	stats.Passed = tPassed + aPassed
	stats.Failed = tFailed + aFailed
	stats.NeedsAttention = tNA + aNA
	if stats.Total > 0 {
		stats.ProgressPct = (stats.Completed * 100) / stats.Total
	}
	return stats, nil
}

// ─── Ad-hoc Items ───────────────────────────────────────────────

// CreateAdhocItemsTables creates the table for ad-hoc inspection items.
func (db *DB) CreateAdhocItemsTables(ctx context.Context) error {
	sql := `
	CREATE TABLE IF NOT EXISTS inspection_adhoc_items (
		id SERIAL PRIMARY KEY,
		inspection_id INTEGER NOT NULL REFERENCES inspections(id) ON DELETE CASCADE,
		category_name VARCHAR(300) NOT NULL DEFAULT 'Ad-hoc Items',
		label VARCHAR(500) NOT NULL,
		description TEXT,
		status VARCHAR(50),
		notes TEXT,
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_adhoc_items_inspection ON inspection_adhoc_items(inspection_id);
	`
	_, err := db.pool.Exec(ctx, sql)
	return err
}

// CreateAdhocItem adds an ad-hoc checklist item to an inspection.
func (db *DB) CreateAdhocItem(ctx context.Context, inspectionID int, label, categoryName string, description *string) (*InspectionChecklistItem, error) {
	if categoryName == "" {
		categoryName = "Ad-hoc Items"
	}

	// Get next sort order for this inspection's adhoc items
	var maxSort int
	_ = db.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(sort_order), 0) FROM inspection_adhoc_items WHERE inspection_id = $1
	`, inspectionID).Scan(&maxSort)

	var id int
	var desc *string
	err := db.pool.QueryRow(ctx, `
		INSERT INTO inspection_adhoc_items (inspection_id, category_name, label, description, sort_order)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, description
	`, inspectionID, categoryName, label, description, maxSort+1).Scan(&id, &desc)
	if err != nil {
		return nil, fmt.Errorf("create adhoc item: %w", err)
	}

	// Touch inspection updated_at
	db.pool.Exec(ctx, `UPDATE inspections SET updated_at = NOW() WHERE id = $1`, inspectionID)

	return &InspectionChecklistItem{
		ItemID:      id,
		Label:       label,
		Description: desc,
		SortOrder:   maxSort + 1,
		IsAdhoc:     true,
	}, nil
}

// UpdateAdhocItemStatus sets or clears the status of an ad-hoc item.
func (db *DB) UpdateAdhocItemStatus(ctx context.Context, inspectionID, adhocID int, status *ItemStatus, notes *string) error {
	var statusStr *string
	if status != nil {
		s := string(*status)
		statusStr = &s
	}
	_, err := db.pool.Exec(ctx, `
		UPDATE inspection_adhoc_items SET status = $1, notes = $2, updated_at = NOW()
		WHERE id = $3 AND inspection_id = $4
	`, statusStr, notes, adhocID, inspectionID)
	if err != nil {
		return fmt.Errorf("update adhoc item status: %w", err)
	}
	db.pool.Exec(ctx, `UPDATE inspections SET updated_at = NOW() WHERE id = $1`, inspectionID)
	return nil
}

// DeleteAdhocItem removes an ad-hoc item from an inspection.
func (db *DB) DeleteAdhocItem(ctx context.Context, inspectionID, adhocID int) error {
	_, err := db.pool.Exec(ctx, `
		DELETE FROM inspection_adhoc_items WHERE id = $1 AND inspection_id = $2
	`, adhocID, inspectionID)
	if err != nil {
		return fmt.Errorf("delete adhoc item: %w", err)
	}
	db.pool.Exec(ctx, `UPDATE inspections SET updated_at = NOW() WHERE id = $1`, inspectionID)
	return nil
}

// GetAdhocItems returns all ad-hoc items for an inspection, grouped by category name.
func (db *DB) GetAdhocItems(ctx context.Context, inspectionID int) ([]InspectionChecklistCategory, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, category_name, label, description, status, notes, sort_order
		FROM inspection_adhoc_items
		WHERE inspection_id = $1
		ORDER BY category_name, sort_order, id
	`, inspectionID)
	if err != nil {
		return nil, fmt.Errorf("get adhoc items: %w", err)
	}
	defer rows.Close()

	catMap := make(map[string]int)
	var categories []InspectionChecklistCategory

	for rows.Next() {
		var (
			id       int
			catName  string
			label    string
			desc     *string
			status   *string
			notes    *string
			sortOrd  int
		)
		if err := rows.Scan(&id, &catName, &label, &desc, &status, &notes, &sortOrd); err != nil {
			return nil, fmt.Errorf("scan adhoc row: %w", err)
		}

		idx, exists := catMap[catName]
		if !exists {
			idx = len(categories)
			catMap[catName] = idx
			categories = append(categories, InspectionChecklistCategory{
				CategoryID: -(idx + 1), // negative IDs to distinguish from template categories
				Name:       catName,
				SortOrder:  9999 + idx, // sort after template categories
				Items:      []InspectionChecklistItem{},
			})
		}

		var itemStatus *ItemStatus
		if status != nil {
			s := ItemStatus(*status)
			itemStatus = &s
		}

		categories[idx].Items = append(categories[idx].Items, InspectionChecklistItem{
			ItemID:        id,
			Label:         label,
			Description:   desc,
			SortOrder:     sortOrd,
			Status:        itemStatus,
			ResponseNotes: notes,
			IsAdhoc:       true,
		})
	}

	return categories, nil
}

// GetAdhocItem returns a single ad-hoc item for re-rendering after status update.
func (db *DB) GetAdhocItem(ctx context.Context, inspectionID, adhocID int) (*InspectionChecklistItem, error) {
	var (
		id      int
		label   string
		desc    *string
		status  *string
		notes   *string
		sortOrd int
	)
	err := db.pool.QueryRow(ctx, `
		SELECT id, label, description, status, notes, sort_order
		FROM inspection_adhoc_items
		WHERE id = $1 AND inspection_id = $2
	`, adhocID, inspectionID).Scan(&id, &label, &desc, &status, &notes, &sortOrd)
	if err != nil {
		return nil, fmt.Errorf("get adhoc item: %w", err)
	}

	var itemStatus *ItemStatus
	if status != nil {
		s := ItemStatus(*status)
		itemStatus = &s
	}

	return &InspectionChecklistItem{
		ItemID:        id,
		Label:         label,
		Description:   desc,
		SortOrder:     sortOrd,
		Status:        itemStatus,
		ResponseNotes: notes,
		IsAdhoc:       true,
	}, nil
}

