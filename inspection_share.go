package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ─── Share Token DB Methods ───────────────────────────────────────

// AddShareTokenColumn adds share_token to inspections if it doesn't exist.
// Safe to run at startup — postgres ADD COLUMN IF NOT EXISTS is idempotent.
func (db *DB) AddShareTokenColumn(ctx context.Context) error {
	_, err := db.pool.Exec(ctx,
		`ALTER TABLE inspections ADD COLUMN IF NOT EXISTS share_token VARCHAR(100) UNIQUE`)
	return err
}

// GetOrCreateShareToken returns the existing share token for an inspection, or generates one.
func (db *DB) GetOrCreateShareToken(ctx context.Context, inspectionID int) (string, error) {
	var token *string
	err := db.pool.QueryRow(ctx,
		`SELECT share_token FROM inspections WHERE id = $1`, inspectionID,
	).Scan(&token)
	if err != nil {
		return "", fmt.Errorf("get inspection: %w", err)
	}
	if token != nil && *token != "" {
		return *token, nil
	}

	// Generate fresh token
	t := uuid.New().String()
	_, err = db.pool.Exec(ctx,
		`UPDATE inspections SET share_token = $1 WHERE id = $2`, t, inspectionID)
	if err != nil {
		return "", fmt.Errorf("set share token: %w", err)
	}
	return t, nil
}

// GetInspectionByShareToken looks up a full inspection checklist by share token.
func (db *DB) GetInspectionByShareToken(ctx context.Context, token string) (*InspectionChecklist, error) {
	var id int
	err := db.pool.QueryRow(ctx,
		`SELECT id FROM inspections WHERE share_token = $1`, token,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("token not found: %w", err)
	}
	return db.GetInspectionChecklist(ctx, id)
}

// ─── Read-only Item Rendering ─────────────────────────────────────

// renderShareItemHTML produces a read-only card for one checklist item (share page).
func renderShareItemHTML(item InspectionChecklistItem, photos []InspectionPhoto) string {
	statusHTML := `<span class="text-[11px] text-gray-400">— Unchecked</span>`
	borderClass := "border-gray-100"
	if item.Status != nil {
		switch *item.Status {
		case ItemStatusPass:
			statusHTML = `<span class="inline-flex items-center gap-1 px-2 py-0.5 text-xs font-bold text-green-700 bg-green-100 rounded-full">✓ Pass</span>`
			borderClass = "border-green-200 bg-green-50/30"
		case ItemStatusFail:
			statusHTML = `<span class="inline-flex items-center gap-1 px-2 py-0.5 text-xs font-bold text-red-700 bg-red-100 rounded-full">✗ Fail</span>`
			borderClass = "border-red-200 bg-red-50/30"
		case ItemStatusNeedsAttention:
			statusHTML = `<span class="inline-flex items-center gap-1 px-2 py-0.5 text-xs font-bold text-amber-700 bg-amber-100 rounded-full">⚠ Attention</span>`
			borderClass = "border-amber-200 bg-amber-50/30"
		}
	}

	descHTML := ""
	if item.Description != nil && *item.Description != "" {
		descHTML = fmt.Sprintf(`<p class="text-xs text-gray-400 mt-0.5">%s</p>`, escapeHTML(*item.Description))
	}

	notesHTML := ""
	if item.ResponseNotes != nil && *item.ResponseNotes != "" {
		notesHTML = fmt.Sprintf(`<div class="mt-1.5 text-xs text-gray-500 italic bg-gray-50 rounded px-2 py-1">%s</div>`, escapeHTML(*item.ResponseNotes))
	}

	adhocHTML := ""
	if item.IsAdhoc {
		adhocHTML = `<span class="text-[10px] text-indigo-600 bg-indigo-50 rounded-full px-2 py-0.5 font-medium ml-1">+ Ad-hoc</span>`
	}

	// Photo strip (thumbnails open in lightbox)
	photosHTML := ""
	if len(photos) > 0 {
		galleryID := fmt.Sprintf("sg-%d", item.ItemID)
		if item.IsAdhoc {
			galleryID = fmt.Sprintf("sg-adhoc-%d", item.ItemID)
		}
		var ph strings.Builder
		ph.WriteString(fmt.Sprintf(`<div id="%s" class="flex flex-wrap gap-2 mt-2">`, galleryID))
		for _, p := range photos {
			ph.WriteString(fmt.Sprintf(
				`<button type="button" onclick="openShareLightbox(%d, '%s')" data-photo-id="%d"
					class="flex-shrink-0 rounded-lg overflow-hidden border border-gray-200 hover:border-duke-teal focus:outline-none focus:ring-2 focus:ring-duke-teal/50 transition-colors">
					<img src="/api/inspections/photos/%d/thumb" alt="Photo" loading="lazy" class="w-16 h-16 object-cover"/>
				</button>`,
				p.ID, galleryID, p.ID, p.ID,
			))
		}
		ph.WriteString(`</div>`)
		photosHTML = ph.String()
	}

	return fmt.Sprintf(`
		<div class="border rounded-xl p-3 %s">
			<div class="flex items-start justify-between gap-2">
				<div class="flex-1 min-w-0">
					<span class="text-sm font-medium text-duke-dark">%s</span>%s
					%s
					%s
				</div>
				<div class="flex-shrink-0 mt-0.5">%s</div>
			</div>
			%s
		</div>`,
		borderClass,
		escapeHTML(item.Label), adhocHTML,
		descHTML,
		notesHTML,
		statusHTML,
		photosHTML,
	)
}

// ─── Print HTML Rendering ─────────────────────────────────────────

// renderPrintHTML builds a complete standalone HTML page for browser print-to-PDF.
func renderPrintHTML(checklist *InspectionChecklist, photosByItem *PhotosByItemResult, host string) string {
	insp := checklist.Inspection
	stats := checklist.Stats

	createdAt := insp.CreatedAt.Format("January 2, 2006")
	completedLine := ""
	if insp.CompletedAt != nil {
		completedLine = fmt.Sprintf(`<span>✓ Completed %s</span>`, insp.CompletedAt.Format("Jan 2, 2006 3:04 PM"))
	}
	statusLabel := map[string]string{
		"completed":   "Completed",
		"in_progress": "In Progress",
		"draft":       "Draft",
	}[insp.Status]
	if statusLabel == "" {
		statusLabel = insp.Status
	}

	// Categories HTML
	var catSB strings.Builder
	for _, cat := range checklist.Categories {
		catSB.WriteString(fmt.Sprintf(`<div class="category"><h2>%s</h2><div class="items">`,
			escapeHTML(cat.Name)))
		for _, item := range cat.Items {
			var photos []InspectionPhoto
			if item.IsAdhoc {
				photos = photosByItem.ByAdhocItemID[item.ItemID]
			} else {
				photos = photosByItem.ByItemID[item.ItemID]
			}
			catSB.WriteString(renderPrintItem(item, photos))
		}
		catSB.WriteString(`</div></div>`)
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Inspection Report — %s</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;font-size:13px;color:#1a1a1a;background:#fff;padding:24px;max-width:860px;margin:0 auto}
.no-print{margin-bottom:20px}
@media print{.no-print{display:none!important}}
.print-btn{display:inline-flex;align-items:center;gap:6px;padding:9px 20px;background:#006778;color:#fff;border:none;border-radius:8px;font-size:13px;font-weight:600;cursor:pointer}
.print-btn:hover{background:#023645}
.report-header{border-bottom:3px solid #006778;padding-bottom:16px;margin-bottom:20px}
.report-header h1{font-size:22px;font-weight:700;color:#023645}
.report-meta{display:flex;flex-wrap:wrap;gap:14px;margin-top:8px;color:#555;font-size:12px}
.stats-row{display:flex;gap:10px;margin-bottom:24px;flex-wrap:wrap}
.stat-box{border:1px solid #e5e7eb;border-radius:8px;padding:10px 16px;text-align:center;min-width:76px}
.stat-box .val{font-size:20px;font-weight:700}
.stat-box .lbl{font-size:10px;color:#888;text-transform:uppercase;letter-spacing:.05em}
.stat-pass .val{color:#15803d}.stat-fail .val{color:#b91c1c}.stat-attn .val{color:#d97706}.stat-total .val{color:#023645}
.category{margin-bottom:24px}
.category h2{font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:.08em;color:#023645;border-bottom:2px solid #006778;padding-bottom:5px;margin-bottom:10px}
.items{display:flex;flex-direction:column;gap:8px}
.item{border:1px solid #e5e7eb;border-radius:8px;padding:10px 12px;page-break-inside:avoid}
.item.pass{border-color:#bbf7d0;background:#f0fdf4}
.item.fail{border-color:#fecaca;background:#fff5f5}
.item.attn{border-color:#fde68a;background:#fffbeb}
.item-row{display:flex;align-items:flex-start;justify-content:space-between;gap:10px}
.item-label{font-weight:600;font-size:13px;flex:1}
.item-desc{font-size:11px;color:#6b7280;margin-top:3px}
.item-notes{font-size:11px;color:#4b5563;font-style:italic;background:#f9fafb;border-radius:4px;padding:4px 8px;margin-top:6px}
.badge{flex-shrink:0;font-size:11px;font-weight:700;padding:2px 10px;border-radius:99px;white-space:nowrap;margin-top:1px}
.badge-pass{color:#15803d;background:#dcfce7}
.badge-fail{color:#b91c1c;background:#fee2e2}
.badge-attn{color:#92400e;background:#fef3c7}
.badge-none{color:#9ca3af}
.badge-adhoc{color:#6366f1;background:#eef2ff;font-size:10px;margin-left:4px;padding:1px 6px}
.photos{display:flex;flex-wrap:wrap;gap:6px;margin-top:8px}
.photos img{width:72px;height:72px;object-fit:cover;border-radius:6px;border:1px solid #e5e7eb}
.report-footer{margin-top:32px;padding-top:12px;border-top:1px solid #e5e7eb;font-size:11px;color:#9ca3af;display:flex;justify-content:space-between;flex-wrap:wrap;gap:8px}
</style>
</head>
<body>
<div class="no-print">
  <button class="print-btn" onclick="window.print()">
    <svg width="16" height="16" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M17 17h2a2 2 0 002-2v-4a2 2 0 00-2-2H5a2 2 0 00-2 2v4a2 2 0 002 2h2m2 4h6a2 2 0 002-2v-4a2 2 0 00-2-2H9a2 2 0 00-2 2v4a2 2 0 002 2zm8-12V5a2 2 0 00-2-2H9a2 2 0 00-2 2v4h10z"/></svg>
    Print / Save as PDF
  </button>
</div>
<div class="report-header">
  <h1>%s</h1>
  <div class="report-meta">
    <span>👤 %s</span>
    <span>📅 %s</span>
    %s
    <span>Status: %s</span>
  </div>
</div>
<div class="stats-row">
  <div class="stat-box stat-total"><div class="val">%d</div><div class="lbl">Total</div></div>
  <div class="stat-box stat-pass"><div class="val">%d</div><div class="lbl">Pass</div></div>
  <div class="stat-box stat-fail"><div class="val">%d</div><div class="lbl">Fail</div></div>
  <div class="stat-box stat-attn"><div class="val">%d</div><div class="lbl">Attention</div></div>
  <div class="stat-box"><div class="val">%d%%</div><div class="lbl">Complete</div></div>
</div>
%s
<div class="report-footer">
  <span>DukeCam Inspection Report · %s</span>
  <span>https://%s/inspection/%d</span>
</div>
</body></html>`,
		escapeHTML(insp.PropertyName),
		escapeHTML(insp.PropertyName),
		escapeHTML(insp.InspectorName),
		createdAt,
		completedLine,
		statusLabel,
		stats.Total, stats.Passed, stats.Failed, stats.NeedsAttention, stats.ProgressPct,
		catSB.String(),
		createdAt,
		host, insp.ID,
	)
}

// renderPrintItem produces one item row for the print page (plain HTML, no Tailwind).
func renderPrintItem(item InspectionChecklistItem, photos []InspectionPhoto) string {
	itemClass := ""
	badge := `<span class="badge badge-none">—</span>`
	if item.Status != nil {
		switch *item.Status {
		case ItemStatusPass:
			itemClass = "pass"
			badge = `<span class="badge badge-pass">✓ Pass</span>`
		case ItemStatusFail:
			itemClass = "fail"
			badge = `<span class="badge badge-fail">✗ Fail</span>`
		case ItemStatusNeedsAttention:
			itemClass = "attn"
			badge = `<span class="badge badge-attn">⚠ Attention</span>`
		}
	}

	adhoc := ""
	if item.IsAdhoc {
		adhoc = `<span class="badge badge-adhoc">Ad-hoc</span>`
	}

	desc := ""
	if item.Description != nil && *item.Description != "" {
		desc = fmt.Sprintf(`<div class="item-desc">%s</div>`, escapeHTML(*item.Description))
	}

	notes := ""
	if item.ResponseNotes != nil && *item.ResponseNotes != "" {
		notes = fmt.Sprintf(`<div class="item-notes">%s</div>`, escapeHTML(*item.ResponseNotes))
	}

	photosHTML := ""
	if len(photos) > 0 {
		var ph strings.Builder
		ph.WriteString(`<div class="photos">`)
		for _, p := range photos {
			ph.WriteString(fmt.Sprintf(`<img src="/api/inspections/photos/%d/thumb" alt="" loading="lazy"/>`, p.ID))
		}
		ph.WriteString(`</div>`)
		photosHTML = ph.String()
	}

	return fmt.Sprintf(`
	<div class="item %s">
	  <div class="item-row">
	    <div style="flex:1">
	      <div class="item-label">%s%s</div>
	      %s
	    </div>
	    %s
	  </div>
	  %s%s
	</div>`,
		itemClass,
		escapeHTML(item.Label), adhoc,
		desc,
		badge,
		notes,
		photosHTML,
	)
}
