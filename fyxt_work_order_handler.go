package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

// ─── Create Fyxt Work Order from Inspection ─────────────────────

// createWorkOrderRequest is the JSON body for the work order creation endpoint.
type createWorkOrderRequest struct {
	Description string  `json:"description"`
	Priority    string  `json:"priority"`
	TargetDate  *string `json:"target_date,omitempty"`
}

// POST /api/inspections/:id/work-order
// Creates a Fyxt work order from the inspection's flagged items.
// Returns JSON with the created job ID and a direct Fyxt URL.
func (a *App) CreateFyxtWorkOrderHandler(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid inspection id"})
	}

	var req createWorkOrderRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}

	req.Description = strings.TrimSpace(req.Description)
	if req.Description == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "description is required"})
	}

	validPriorities := map[string]bool{"Low": true, "Medium": true, "High": true, "Emergency": true}
	if req.Priority == "" {
		req.Priority = "Medium"
	}
	if !validPriorities[req.Priority] {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid priority"})
	}

	if !a.fyxt.IsConfigured() {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "Fyxt integration not configured"})
	}
	if !a.propertyOS.IsConfigured() {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "PropertyOS not configured"})
	}

	// Load the inspection to get property + unit context
	insp, err := a.db.GetInspectionByID(ctx, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "inspection not found"})
	}

	// Look up the building from the PropertyOS buildings list (cached; supports service auth).
	// We only need fyxt_property_id and address — no need for the individual building endpoint.
	buildingOSID := insp.PropertyID
	if insp.BuildingID != nil && *insp.BuildingID > 0 {
		buildingOSID = *insp.BuildingID
	}

	buildings, err := a.propertyOS.ListBuildings(ctx)
	if err != nil {
		log.Printf("CreateFyxtWorkOrder: PropertyOS list error: %v", err)
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "Could not reach PropertyOS to resolve property"})
	}

	var building *Building
	for i := range buildings {
		if buildings[i].ID == buildingOSID {
			building = &buildings[i]
			break
		}
	}

	if building == nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("Building ID %d not found in PropertyOS", buildingOSID),
		})
	}

	if building.FyxtPropertyID == nil || *building.FyxtPropertyID == "" {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{
			"error": fmt.Sprintf("Property %q is not linked to Fyxt — set fyxt_property_id in PropertyOS", building.Name),
		})
	}

	requestType := "Common Area"
	if insp.UnitID != nil && *insp.UnitID > 0 {
		requestType = "In Unit"
	}

	// Fyxt requires a unit_id — look up the best default for this property.
	fyxtUnitID, err := a.fyxt.GetDefaultUnitForProperty(ctx, *building.FyxtPropertyID)
	if err != nil {
		log.Printf("CreateFyxtWorkOrder: unit lookup failed for %s: %v", *building.FyxtPropertyID, err)
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "Could not look up Fyxt units for this property"})
	}
	if fyxtUnitID == "" {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "No units found in Fyxt for this property"})
	}

	payload := WorkOrderPayload{
		PropertyID:  *building.FyxtPropertyID,
		UnitID:      fyxtUnitID,
		Address:     building.Address,
		Description: req.Description,
		Priorities:  req.Priority,
		RequestType: requestType,
		SkipBid:     true,
		NotifyTenant: false,
		TargetDate:  req.TargetDate,
	}

	result, err := a.fyxt.CreateWorkOrder(ctx, payload)
	if err != nil {
		log.Printf("CreateFyxtWorkOrder: Fyxt API error for inspection %d: %v", id, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "Failed to create work order: " + err.Error(),
		})
	}

	log.Printf("Fyxt work order #%s created from inspection %d (%s / Fyxt property %s)",
		result.ID, id, insp.PropertyName, *building.FyxtPropertyID)

	return c.JSON(http.StatusCreated, map[string]interface{}{
		"job_id":  result.ID,
		"fyxt_url": fmt.Sprintf("https://thomasduke.fyxt.com/maintenance/work-orders/%s", result.ID),
		"message": fmt.Sprintf("Work order #%s created in Fyxt", result.ID),
	})
}
