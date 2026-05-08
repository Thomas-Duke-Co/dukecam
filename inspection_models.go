package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// ─── Inspection Template Models ─────────────────────────────────

// InspectionTemplate is a reusable checklist blueprint (e.g. "Move-In Inspection", "Annual Property Review").
type InspectionTemplate struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	Description *string    `json:"description,omitempty"`
	Active      bool       `json:"active"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Categories  []InspectionTemplateCategory `json:"categories,omitempty"`
}

// InspectionTemplateCategory groups related checklist items within a template (e.g. "Kitchen", "HVAC", "Exterior").
type InspectionTemplateCategory struct {
	ID         int       `json:"id"`
	TemplateID int       `json:"template_id"`
	Name       string    `json:"name"`
	SortOrder  int       `json:"sort_order"`
	CreatedAt  time.Time `json:"created_at"`
	Items      []InspectionTemplateItem `json:"items,omitempty"`
}

// InspectionTemplateItem is a single checklist line within a category (e.g. "Smoke detector functional").
type InspectionTemplateItem struct {
	ID           int       `json:"id"`
	CategoryID   int       `json:"category_id"`
	Label        string    `json:"label"`
	Description  *string   `json:"description,omitempty"`
	RequirePhoto bool      `json:"require_photo"`
	SortOrder    int       `json:"sort_order"`
	CreatedAt    time.Time `json:"created_at"`
}

// AdminInspectionTemplate is used in admin list views with aggregate counts.
type AdminInspectionTemplate struct {
	InspectionTemplate
	CategoryCount int
	ItemCount     int
}

// ─── Inspection Template Table Creation ─────────────────────────

func (db *DB) CreateInspectionTables(ctx context.Context) error {
	sql := `
	CREATE TABLE IF NOT EXISTS inspection_templates (
		id SERIAL PRIMARY KEY,
		name VARCHAR(300) NOT NULL,
		description TEXT,
		active BOOLEAN NOT NULL DEFAULT true,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE TABLE IF NOT EXISTS inspection_template_categories (
		id SERIAL PRIMARY KEY,
		template_id INTEGER NOT NULL REFERENCES inspection_templates(id) ON DELETE CASCADE,
		name VARCHAR(300) NOT NULL,
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_insp_cat_template ON inspection_template_categories(template_id);

	CREATE TABLE IF NOT EXISTS inspection_template_items (
		id SERIAL PRIMARY KEY,
		category_id INTEGER NOT NULL REFERENCES inspection_template_categories(id) ON DELETE CASCADE,
		label VARCHAR(500) NOT NULL,
		description TEXT,
		require_photo BOOLEAN NOT NULL DEFAULT false,
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_insp_item_category ON inspection_template_items(category_id);
	`
	_, err := db.pool.Exec(ctx, sql)
	return err
}

// ─── Inspector-Facing Template Queries ──────────────────────────

// ActiveInspectionTemplate is returned to inspectors when choosing a template to start.
type ActiveInspectionTemplate struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Description   *string `json:"description,omitempty"`
	CategoryCount int    `json:"category_count"`
	ItemCount     int    `json:"item_count"`
}

// ListActiveInspectionTemplates returns only active templates with category/item counts.
// Used by the inspector flow to choose a template when starting a new inspection.
func (db *DB) ListActiveInspectionTemplates(ctx context.Context) ([]ActiveInspectionTemplate, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT t.id, t.name, t.description,
		       COALESCE(c.cat_count, 0),
		       COALESCE(i.item_count, 0)
		FROM inspection_templates t
		LEFT JOIN (
			SELECT template_id, COUNT(*) as cat_count
			FROM inspection_template_categories GROUP BY template_id
		) c ON t.id = c.template_id
		LEFT JOIN (
			SELECT tc.template_id, COUNT(*) as item_count
			FROM inspection_template_items ti
			JOIN inspection_template_categories tc ON ti.category_id = tc.id
			GROUP BY tc.template_id
		) i ON t.id = i.template_id
		WHERE t.active = true
		ORDER BY t.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []ActiveInspectionTemplate
	for rows.Next() {
		var t ActiveInspectionTemplate
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.CategoryCount, &t.ItemCount); err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// GetInspectionTemplateChecklist loads a full template with all categories and items,
// suitable for rendering the inspector checklist. Only returns the template if it's active.
func (db *DB) GetInspectionTemplateChecklist(ctx context.Context, id int) (*InspectionTemplate, error) {
	// Verify template exists and is active
	var t InspectionTemplate
	err := db.pool.QueryRow(ctx, `
		SELECT id, name, description, active, created_at, updated_at
		FROM inspection_templates WHERE id = $1 AND active = true
	`, id).Scan(&t.ID, &t.Name, &t.Description, &t.Active, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}

	// Load categories
	catRows, err := db.pool.Query(ctx, `
		SELECT id, template_id, name, sort_order, created_at
		FROM inspection_template_categories
		WHERE template_id = $1
		ORDER BY sort_order, id
	`, id)
	if err != nil {
		return nil, err
	}
	defer catRows.Close()

	catMap := make(map[int]int) // category ID -> index in slice
	for catRows.Next() {
		var c InspectionTemplateCategory
		if err := catRows.Scan(&c.ID, &c.TemplateID, &c.Name, &c.SortOrder, &c.CreatedAt); err != nil {
			return nil, err
		}
		catMap[c.ID] = len(t.Categories)
		t.Categories = append(t.Categories, c)
	}

	if len(t.Categories) == 0 {
		return &t, nil
	}

	// Load all items for all categories in one query
	itemRows, err := db.pool.Query(ctx, `
		SELECT i.id, i.category_id, i.label, i.description, i.require_photo, i.sort_order, i.created_at
		FROM inspection_template_items i
		JOIN inspection_template_categories c ON i.category_id = c.id
		WHERE c.template_id = $1
		ORDER BY c.sort_order, c.id, i.sort_order, i.id
	`, id)
	if err != nil {
		return nil, err
	}
	defer itemRows.Close()

	for itemRows.Next() {
		var item InspectionTemplateItem
		if err := itemRows.Scan(&item.ID, &item.CategoryID, &item.Label, &item.Description, &item.RequirePhoto, &item.SortOrder, &item.CreatedAt); err != nil {
			return nil, err
		}
		if idx, ok := catMap[item.CategoryID]; ok {
			t.Categories[idx].Items = append(t.Categories[idx].Items, item)
		}
	}

	return &t, nil
}

// ─── Template Queries ───────────────────────────────────────────

func (db *DB) ListInspectionTemplates(ctx context.Context) ([]InspectionTemplate, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, name, description, active, created_at, updated_at
		FROM inspection_templates
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []InspectionTemplate
	for rows.Next() {
		var t InspectionTemplate
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.Active, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// ListInspectionTemplatesAdmin returns templates with category and item counts for admin views.
func (db *DB) ListInspectionTemplatesAdmin(ctx context.Context) ([]AdminInspectionTemplate, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT t.id, t.name, t.description, t.active, t.created_at, t.updated_at,
		       COALESCE(c.cat_count, 0),
		       COALESCE(i.item_count, 0)
		FROM inspection_templates t
		LEFT JOIN (
			SELECT template_id, COUNT(*) as cat_count
			FROM inspection_template_categories GROUP BY template_id
		) c ON t.id = c.template_id
		LEFT JOIN (
			SELECT tc.template_id, COUNT(*) as item_count
			FROM inspection_template_items ti
			JOIN inspection_template_categories tc ON ti.category_id = tc.id
			GROUP BY tc.template_id
		) i ON t.id = i.template_id
		ORDER BY t.active DESC, t.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []AdminInspectionTemplate
	for rows.Next() {
		var at AdminInspectionTemplate
		err := rows.Scan(
			&at.ID, &at.Name, &at.Description, &at.Active,
			&at.CreatedAt, &at.UpdatedAt,
			&at.CategoryCount, &at.ItemCount,
		)
		if err != nil {
			return nil, err
		}
		templates = append(templates, at)
	}
	return templates, nil
}

// GetCategoryWithItems loads a single category with its items.
func (db *DB) GetCategoryWithItems(ctx context.Context, id int) (*InspectionTemplateCategory, error) {
	var c InspectionTemplateCategory
	err := db.pool.QueryRow(ctx, `
		SELECT id, template_id, name, sort_order, created_at
		FROM inspection_template_categories WHERE id = $1
	`, id).Scan(&c.ID, &c.TemplateID, &c.Name, &c.SortOrder, &c.CreatedAt)
	if err != nil {
		return nil, err
	}

	itemRows, err := db.pool.Query(ctx, `
		SELECT id, category_id, label, description, require_photo, sort_order, created_at
		FROM inspection_template_items WHERE category_id = $1
		ORDER BY sort_order, id
	`, id)
	if err != nil {
		return nil, err
	}
	defer itemRows.Close()

	for itemRows.Next() {
		var item InspectionTemplateItem
		if err := itemRows.Scan(&item.ID, &item.CategoryID, &item.Label, &item.Description, &item.RequirePhoto, &item.SortOrder, &item.CreatedAt); err != nil {
			return nil, err
		}
		c.Items = append(c.Items, item)
	}

	return &c, nil
}

func (db *DB) GetInspectionTemplateByID(ctx context.Context, id int) (*InspectionTemplate, error) {
	var t InspectionTemplate
	err := db.pool.QueryRow(ctx, `
		SELECT id, name, description, active, created_at, updated_at
		FROM inspection_templates WHERE id = $1
	`, id).Scan(&t.ID, &t.Name, &t.Description, &t.Active, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetInspectionTemplateFull loads a template with all its categories and items in a single call.
func (db *DB) GetInspectionTemplateFull(ctx context.Context, id int) (*InspectionTemplate, error) {
	t, err := db.GetInspectionTemplateByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Load categories
	catRows, err := db.pool.Query(ctx, `
		SELECT id, template_id, name, sort_order, created_at
		FROM inspection_template_categories
		WHERE template_id = $1
		ORDER BY sort_order, id
	`, id)
	if err != nil {
		return nil, err
	}
	defer catRows.Close()

	catMap := make(map[int]int) // category ID -> index in slice
	for catRows.Next() {
		var c InspectionTemplateCategory
		if err := catRows.Scan(&c.ID, &c.TemplateID, &c.Name, &c.SortOrder, &c.CreatedAt); err != nil {
			return nil, err
		}
		catMap[c.ID] = len(t.Categories)
		t.Categories = append(t.Categories, c)
	}

	if len(t.Categories) == 0 {
		return t, nil
	}

	// Load all items for all categories in one query
	itemRows, err := db.pool.Query(ctx, `
		SELECT i.id, i.category_id, i.label, i.description, i.require_photo, i.sort_order, i.created_at
		FROM inspection_template_items i
		JOIN inspection_template_categories c ON i.category_id = c.id
		WHERE c.template_id = $1
		ORDER BY i.sort_order, i.id
	`, id)
	if err != nil {
		return nil, err
	}
	defer itemRows.Close()

	for itemRows.Next() {
		var item InspectionTemplateItem
		if err := itemRows.Scan(&item.ID, &item.CategoryID, &item.Label, &item.Description, &item.RequirePhoto, &item.SortOrder, &item.CreatedAt); err != nil {
			return nil, err
		}
		if idx, ok := catMap[item.CategoryID]; ok {
			t.Categories[idx].Items = append(t.Categories[idx].Items, item)
		}
	}

	return t, nil
}

func (db *DB) CreateInspectionTemplate(ctx context.Context, name string, description *string) (*InspectionTemplate, error) {
	var t InspectionTemplate
	err := db.pool.QueryRow(ctx, `
		INSERT INTO inspection_templates (name, description)
		VALUES ($1, $2)
		RETURNING id, name, description, active, created_at, updated_at
	`, name, description).Scan(&t.ID, &t.Name, &t.Description, &t.Active, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (db *DB) UpdateInspectionTemplate(ctx context.Context, id int, name string, description *string) (*InspectionTemplate, error) {
	var t InspectionTemplate
	err := db.pool.QueryRow(ctx, `
		UPDATE inspection_templates
		SET name = $2, description = $3, updated_at = NOW()
		WHERE id = $1
		RETURNING id, name, description, active, created_at, updated_at
	`, id, name, description).Scan(&t.ID, &t.Name, &t.Description, &t.Active, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (db *DB) ToggleInspectionTemplateActive(ctx context.Context, id int) (*InspectionTemplate, error) {
	var t InspectionTemplate
	err := db.pool.QueryRow(ctx, `
		UPDATE inspection_templates SET active = NOT active, updated_at = NOW()
		WHERE id = $1
		RETURNING id, name, description, active, created_at, updated_at
	`, id).Scan(&t.ID, &t.Name, &t.Description, &t.Active, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (db *DB) DeleteInspectionTemplate(ctx context.Context, id int) error {
	_, err := db.pool.Exec(ctx, `DELETE FROM inspection_templates WHERE id = $1`, id)
	return err
}

// ─── Category Queries ───────────────────────────────────────────

func (db *DB) ListCategoriesForTemplate(ctx context.Context, templateID int) ([]InspectionTemplateCategory, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, template_id, name, sort_order, created_at
		FROM inspection_template_categories
		WHERE template_id = $1
		ORDER BY sort_order, id
	`, templateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cats []InspectionTemplateCategory
	for rows.Next() {
		var c InspectionTemplateCategory
		if err := rows.Scan(&c.ID, &c.TemplateID, &c.Name, &c.SortOrder, &c.CreatedAt); err != nil {
			return nil, err
		}
		cats = append(cats, c)
	}
	return cats, nil
}

func (db *DB) CreateInspectionCategory(ctx context.Context, templateID int, name string, sortOrder int) (*InspectionTemplateCategory, error) {
	var c InspectionTemplateCategory
	err := db.pool.QueryRow(ctx, `
		INSERT INTO inspection_template_categories (template_id, name, sort_order)
		VALUES ($1, $2, $3)
		RETURNING id, template_id, name, sort_order, created_at
	`, templateID, name, sortOrder).Scan(&c.ID, &c.TemplateID, &c.Name, &c.SortOrder, &c.CreatedAt)
	if err != nil {
		return nil, err
	}

	// Touch parent template updated_at
	db.pool.Exec(ctx, `UPDATE inspection_templates SET updated_at = NOW() WHERE id = $1`, templateID)

	return &c, nil
}

func (db *DB) UpdateInspectionCategory(ctx context.Context, id int, name string, sortOrder int) (*InspectionTemplateCategory, error) {
	var c InspectionTemplateCategory
	err := db.pool.QueryRow(ctx, `
		UPDATE inspection_template_categories
		SET name = $2, sort_order = $3
		WHERE id = $1
		RETURNING id, template_id, name, sort_order, created_at
	`, id, name, sortOrder).Scan(&c.ID, &c.TemplateID, &c.Name, &c.SortOrder, &c.CreatedAt)
	if err != nil {
		return nil, err
	}

	// Touch parent template updated_at
	db.pool.Exec(ctx, `UPDATE inspection_templates SET updated_at = NOW() WHERE id = $1`, c.TemplateID)

	return &c, nil
}

func (db *DB) DeleteInspectionCategory(ctx context.Context, id int) error {
	// Get template ID before deleting so we can touch updated_at
	var templateID int
	err := db.pool.QueryRow(ctx, `SELECT template_id FROM inspection_template_categories WHERE id = $1`, id).Scan(&templateID)
	if err != nil {
		return err
	}

	_, err = db.pool.Exec(ctx, `DELETE FROM inspection_template_categories WHERE id = $1`, id)
	if err != nil {
		return err
	}

	db.pool.Exec(ctx, `UPDATE inspection_templates SET updated_at = NOW() WHERE id = $1`, templateID)
	return nil
}

// ─── Item Queries ───────────────────────────────────────────────

func (db *DB) ListItemsForCategory(ctx context.Context, categoryID int) ([]InspectionTemplateItem, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, category_id, label, description, require_photo, sort_order, created_at
		FROM inspection_template_items
		WHERE category_id = $1
		ORDER BY sort_order, id
	`, categoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []InspectionTemplateItem
	for rows.Next() {
		var item InspectionTemplateItem
		if err := rows.Scan(&item.ID, &item.CategoryID, &item.Label, &item.Description, &item.RequirePhoto, &item.SortOrder, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (db *DB) CreateInspectionItem(ctx context.Context, categoryID int, label string, description *string, requirePhoto bool, sortOrder int) (*InspectionTemplateItem, error) {
	var item InspectionTemplateItem
	err := db.pool.QueryRow(ctx, `
		INSERT INTO inspection_template_items (category_id, label, description, require_photo, sort_order)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, category_id, label, description, require_photo, sort_order, created_at
	`, categoryID, label, description, requirePhoto, sortOrder).Scan(
		&item.ID, &item.CategoryID, &item.Label, &item.Description, &item.RequirePhoto, &item.SortOrder, &item.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	// Touch parent template updated_at
	db.pool.Exec(ctx, `
		UPDATE inspection_templates SET updated_at = NOW()
		WHERE id = (SELECT template_id FROM inspection_template_categories WHERE id = $1)
	`, categoryID)

	return &item, nil
}

func (db *DB) UpdateInspectionItem(ctx context.Context, id int, label string, description *string, requirePhoto bool, sortOrder int) (*InspectionTemplateItem, error) {
	var item InspectionTemplateItem
	err := db.pool.QueryRow(ctx, `
		UPDATE inspection_template_items
		SET label = $2, description = $3, require_photo = $4, sort_order = $5
		WHERE id = $1
		RETURNING id, category_id, label, description, require_photo, sort_order, created_at
	`, id, label, description, requirePhoto, sortOrder).Scan(
		&item.ID, &item.CategoryID, &item.Label, &item.Description, &item.RequirePhoto, &item.SortOrder, &item.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	// Touch parent template updated_at
	db.pool.Exec(ctx, `
		UPDATE inspection_templates SET updated_at = NOW()
		WHERE id = (SELECT template_id FROM inspection_template_categories WHERE id = $1)
	`, item.CategoryID)

	return &item, nil
}

func (db *DB) DeleteInspectionItem(ctx context.Context, id int) error {
	// Get category ID to touch parent template
	var categoryID int
	err := db.pool.QueryRow(ctx, `SELECT category_id FROM inspection_template_items WHERE id = $1`, id).Scan(&categoryID)
	if err != nil {
		return err
	}

	_, err = db.pool.Exec(ctx, `DELETE FROM inspection_template_items WHERE id = $1`, id)
	if err != nil {
		return err
	}

	db.pool.Exec(ctx, `
		UPDATE inspection_templates SET updated_at = NOW()
		WHERE id = (SELECT template_id FROM inspection_template_categories WHERE id = $1)
	`, categoryID)
	return nil
}

// ─── Seed Default Templates ─────────────────────────────────────

// seedTemplateData defines a template with its categories and items for seeding.
type seedTemplateData struct {
	Name        string
	Description string
	Categories  []seedCategoryData
}

type seedCategoryData struct {
	Name  string
	Items []seedItemData
}

type seedItemData struct {
	Label        string
	Description  string
	RequirePhoto bool
}

// SeedDefaultInspectionTemplates inserts default inspection templates if none exist.
// This is idempotent — it only seeds when the inspection_templates table is empty.
func (db *DB) SeedDefaultInspectionTemplates(ctx context.Context) error {
	// Check if any templates already exist
	var count int
	err := db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM inspection_templates`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check existing templates: %w", err)
	}
	if count > 0 {
		return nil // Already seeded
	}

	templates := defaultInspectionTemplates()

	for _, tmplData := range templates {
		desc := tmplData.Description
		tmpl, err := db.CreateInspectionTemplate(ctx, tmplData.Name, &desc)
		if err != nil {
			return fmt.Errorf("seed template %q: %w", tmplData.Name, err)
		}

		for catIdx, catData := range tmplData.Categories {
			cat, err := db.CreateInspectionCategory(ctx, tmpl.ID, catData.Name, catIdx+1)
			if err != nil {
				return fmt.Errorf("seed category %q in %q: %w", catData.Name, tmplData.Name, err)
			}

			for itemIdx, itemData := range catData.Items {
				var descPtr *string
				if itemData.Description != "" {
					d := itemData.Description
					descPtr = &d
				}
				_, err := db.CreateInspectionItem(ctx, cat.ID, itemData.Label, descPtr, itemData.RequirePhoto, itemIdx+1)
				if err != nil {
					return fmt.Errorf("seed item %q: %w", itemData.Label, err)
				}
			}
		}
	}

	log.Printf("Seeded %d default inspection templates", len(templates))
	return nil
}

// defaultInspectionTemplates returns the built-in template definitions.
func defaultInspectionTemplates() []seedTemplateData {
	return []seedTemplateData{
		{
			Name:        "General Property Inspection",
			Description: "Standard property walkthrough covering all major building systems and areas",
			Categories: []seedCategoryData{
				{
					Name: "Exterior",
					Items: []seedItemData{
						{Label: "Building facade condition", Description: "Check for cracks, damage, or deterioration"},
						{Label: "Parking lot / driveways", Description: "Surface condition, striping, ADA compliance"},
						{Label: "Landscaping and grounds", Description: "Grass, shrubs, trees, irrigation"},
						{Label: "Exterior lighting", Description: "All fixtures functional, adequate coverage"},
						{Label: "Signage condition", Description: "Building and tenant signage intact and visible"},
						{Label: "Sidewalks and curbs", Description: "Trip hazards, ADA ramps, condition"},
						{Label: "Dumpster / waste area", Description: "Clean, accessible, enclosure intact", RequirePhoto: true},
					},
				},
				{
					Name: "Roof",
					Items: []seedItemData{
						{Label: "Roof surface condition", Description: "Membrane, shingles, or flat roof condition", RequirePhoto: true},
						{Label: "Drainage and gutters", Description: "Clear of debris, properly draining"},
						{Label: "Flashing and seals", Description: "Around vents, pipes, and edges"},
						{Label: "Skylights / roof penetrations", Description: "Sealed, no leaks or damage"},
					},
				},
				{
					Name: "Common Areas",
					Items: []seedItemData{
						{Label: "Lobby / entrance condition", Description: "Clean, well-lit, professional appearance"},
						{Label: "Hallways and corridors", Description: "Flooring, walls, ceiling condition"},
						{Label: "Elevators operational", Description: "All elevators working, inspection current"},
						{Label: "Stairwells", Description: "Clean, well-lit, handrails secure"},
						{Label: "Restrooms (common)", Description: "Clean, functional, stocked"},
					},
				},
				{
					Name: "Mechanical / HVAC",
					Items: []seedItemData{
						{Label: "HVAC units operational", Description: "Heating and cooling functional"},
						{Label: "Air filters", Description: "Clean or recently replaced"},
						{Label: "Thermostat function", Description: "Responding correctly to settings"},
						{Label: "Mechanical room condition", Description: "Clean, organized, no leaks", RequirePhoto: true},
						{Label: "Ductwork condition", Description: "No visible damage or disconnections"},
					},
				},
				{
					Name: "Plumbing",
					Items: []seedItemData{
						{Label: "Water heater condition", Description: "Operational, no leaks, proper temperature"},
						{Label: "Visible pipe condition", Description: "No leaks, corrosion, or damage"},
						{Label: "Drains functioning", Description: "No slow drains or backups"},
						{Label: "Water pressure adequate", Description: "Consistent pressure at all fixtures"},
					},
				},
				{
					Name: "Electrical",
					Items: []seedItemData{
						{Label: "Electrical panels accessible", Description: "Clear 36-inch clearance, labeled breakers"},
						{Label: "Outlets and switches", Description: "All functional, covers intact"},
						{Label: "Interior lighting", Description: "All fixtures working, adequate illumination"},
						{Label: "Emergency / exit lighting", Description: "Functional, batteries charged"},
					},
				},
			},
		},
		{
			Name:        "Move-In / Move-Out Inspection",
			Description: "Detailed unit condition assessment for tenant move-in or move-out documentation",
			Categories: []seedCategoryData{
				{
					Name: "Entry / Foyer",
					Items: []seedItemData{
						{Label: "Front door condition", Description: "Opens/closes properly, lock functional, weatherstrip intact"},
						{Label: "Door hardware", Description: "Knob, deadbolt, hinges, peephole"},
						{Label: "Entry flooring", Description: "Condition, stains, damage", RequirePhoto: true},
						{Label: "Entry walls and ceiling", Description: "Paint, holes, scuffs, damage"},
						{Label: "Light switch / fixture", Description: "Functional, clean"},
						{Label: "Closet (if applicable)", Description: "Door, shelf, rod, condition"},
					},
				},
				{
					Name: "Kitchen",
					Items: []seedItemData{
						{Label: "Countertops", Description: "Surface condition, chips, stains, burns", RequirePhoto: true},
						{Label: "Cabinets and drawers", Description: "Doors align, hinges tight, pulls intact"},
						{Label: "Sink and faucet", Description: "No leaks, drains properly, sprayer works"},
						{Label: "Garbage disposal", Description: "Operational, no jams or odors"},
						{Label: "Dishwasher", Description: "Runs full cycle, no leaks, clean interior"},
						{Label: "Stove / oven", Description: "All burners work, oven heats, clean", RequirePhoto: true},
						{Label: "Refrigerator", Description: "Cools properly, ice maker, clean, seals intact"},
						{Label: "Microwave (if built-in)", Description: "Operational, clean, light and fan work"},
						{Label: "Kitchen flooring", Description: "Condition, stains, damage"},
						{Label: "Kitchen walls and ceiling", Description: "Paint, grease marks, damage"},
						{Label: "Kitchen lighting", Description: "All fixtures functional"},
						{Label: "Exhaust fan / range hood", Description: "Operational, filter clean"},
					},
				},
				{
					Name: "Living / Dining Room",
					Items: []seedItemData{
						{Label: "Flooring condition", Description: "Carpet, hardwood, or tile condition", RequirePhoto: true},
						{Label: "Walls and ceiling", Description: "Paint, holes, cracks, stains"},
						{Label: "Windows", Description: "Open/close, locks, screens, blinds"},
						{Label: "Outlets and switches", Description: "All functional, cover plates intact"},
						{Label: "Light fixtures", Description: "All working, clean"},
						{Label: "Ceiling fan (if applicable)", Description: "Operational on all speeds, balanced"},
					},
				},
				{
					Name: "Bedrooms",
					Items: []seedItemData{
						{Label: "Flooring condition", Description: "Carpet, hardwood, or tile condition", RequirePhoto: true},
						{Label: "Walls and ceiling", Description: "Paint, holes, cracks, stains"},
						{Label: "Windows", Description: "Open/close, locks, screens, blinds"},
						{Label: "Closet", Description: "Door, shelf, rod, light, condition", RequirePhoto: true},
						{Label: "Outlets and switches", Description: "All functional, cover plates intact"},
						{Label: "Light fixtures", Description: "All working, clean"},
						{Label: "Smoke detector", Description: "Present, tested, battery current"},
					},
				},
				{
					Name: "Bathrooms",
					Items: []seedItemData{
						{Label: "Toilet", Description: "Flushes properly, no leaks, clean, seat secure"},
						{Label: "Bathtub / shower", Description: "Drain, faucet, showerhead, caulking, tile", RequirePhoto: true},
						{Label: "Sink and vanity", Description: "Faucet, drain, cabinet condition"},
						{Label: "Mirror and medicine cabinet", Description: "Condition, mounting secure"},
						{Label: "Exhaust fan", Description: "Operational, not excessively noisy"},
						{Label: "Flooring", Description: "Tile, vinyl condition, grout, caulk"},
						{Label: "Walls and ceiling", Description: "Paint, mold, moisture damage"},
						{Label: "Towel bars / accessories", Description: "Secure, not damaged"},
					},
				},
				{
					Name: "Laundry Area",
					Items: []seedItemData{
						{Label: "Washer connections", Description: "Hot/cold supply, drain, no leaks"},
						{Label: "Dryer connections", Description: "Vent clear, electrical/gas connection"},
						{Label: "Flooring condition", Description: "No water damage, proper drainage"},
					},
				},
				{
					Name: "General Unit Condition",
					Items: []seedItemData{
						{Label: "All keys provided", Description: "Front door, mailbox, storage, amenity keys"},
						{Label: "Smoke detectors", Description: "Present in all required locations, tested"},
						{Label: "Carbon monoxide detector", Description: "Present if required, tested"},
						{Label: "HVAC filter", Description: "Clean or new filter installed"},
						{Label: "Overall cleanliness", Description: "Unit move-in ready or charges needed", RequirePhoto: true},
					},
				},
			},
		},
		{
			Name:        "Safety & Compliance Inspection",
			Description: "Life safety systems and regulatory compliance verification",
			Categories: []seedCategoryData{
				{
					Name: "Fire Safety",
					Items: []seedItemData{
						{Label: "Fire extinguishers current", Description: "Inspected within 12 months, properly mounted, accessible", RequirePhoto: true},
						{Label: "Fire alarm panel status", Description: "No trouble signals, system operational"},
						{Label: "Sprinkler system", Description: "No obstructions, heads intact, valve positions correct"},
						{Label: "Fire doors self-closing", Description: "All fire-rated doors close and latch properly"},
						{Label: "Exit signs illuminated", Description: "All exit signs visible and lit"},
						{Label: "Emergency lighting functional", Description: "Battery backup tested, adequate illumination"},
						{Label: "Fire escape / egress routes", Description: "Clear, unobstructed, properly marked"},
						{Label: "Smoke detectors functional", Description: "Tested, no expired units"},
						{Label: "Fire suppression (kitchen)", Description: "Hood system inspected, current tag", RequirePhoto: true},
					},
				},
				{
					Name: "Electrical Safety",
					Items: []seedItemData{
						{Label: "Electrical panel clearance", Description: "36-inch clearance maintained, no storage in front"},
						{Label: "GFCI outlets near water", Description: "All GFCI outlets test and reset properly"},
						{Label: "No exposed wiring", Description: "All junction boxes covered, no visible wire damage"},
						{Label: "Extension cord usage", Description: "No permanent extension cord use, no daisy-chaining"},
						{Label: "Surge protection", Description: "Critical systems protected"},
					},
				},
				{
					Name: "ADA Compliance",
					Items: []seedItemData{
						{Label: "Accessible entrance", Description: "Ramp, automatic door, proper signage"},
						{Label: "Accessible restroom", Description: "Grab bars, clearances, signage", RequirePhoto: true},
						{Label: "Accessible parking", Description: "Correct number, proper signage, van-accessible spaces"},
						{Label: "Path of travel clear", Description: "No obstructions, proper width maintained"},
						{Label: "Elevator ADA features", Description: "Braille, audible signals, door timing"},
					},
				},
				{
					Name: "Hazardous Materials",
					Items: []seedItemData{
						{Label: "Asbestos management", Description: "Known ACM labeled, management plan current"},
						{Label: "Chemical storage", Description: "Properly labeled, SDS sheets available, secondary containment"},
						{Label: "Lead paint disclosure", Description: "Proper disclosure for pre-1978 buildings"},
						{Label: "Mold / moisture issues", Description: "No visible mold growth, moisture sources addressed", RequirePhoto: true},
					},
				},
				{
					Name: "Security",
					Items: []seedItemData{
						{Label: "Exterior doors secure", Description: "All locks functional, closers working, no propping"},
						{Label: "Security cameras operational", Description: "All cameras recording, coverage adequate"},
						{Label: "Lighting adequate", Description: "Parking lot, walkways, entries well-lit"},
						{Label: "Access control system", Description: "Key fobs, cards, or codes working properly"},
						{Label: "Emergency contact info posted", Description: "Current emergency numbers visible in common areas"},
					},
				},
				{
					Name: "Environmental",
					Items: []seedItemData{
						{Label: "Backflow preventer tested", Description: "Annual test current, results on file"},
						{Label: "Grease trap maintenance", Description: "Cleaned per schedule, records maintained"},
						{Label: "Stormwater management", Description: "Drains clear, BMPs maintained"},
						{Label: "Waste disposal compliance", Description: "Proper separation, recycling, hazmat disposal"},
					},
				},
			},
		},
	}
}
