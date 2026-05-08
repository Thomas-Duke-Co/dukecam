package main

import (
	"log"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
)

// ─── PropertyOS Inspection Handlers ─────────────────────────────
// These handlers proxy PropertyOS data for the inspector-facing UI.

// GET /api/inspections/properties — list properties from PropertyOS for the property picker.
// Returns JSON array of buildings sorted alphabetically.
// Gracefully returns an empty array if PropertyOS is unreachable.
func (a *App) ListProperties(c echo.Context) error {
	ctx := c.Request().Context()

	if !a.propertyOS.IsConfigured() {
		return c.JSON(http.StatusOK, []interface{}{})
	}

	buildings, err := a.propertyOS.ListBuildings(ctx)
	if err != nil {
		log.Printf("PropertyOS buildings fetch error: %v", err)
		return c.JSON(http.StatusOK, []interface{}{})
	}

	// Shape response for the dropdown — minimal payload for mobile
	type propertyResponse struct {
		ID           int    `json:"id"`
		Name         string `json:"name"`
		Address      string `json:"address"`
		City         string `json:"city"`
		State        string `json:"state"`
		Zip          string `json:"zip"`
		PropertyType string `json:"property_type"`
		FullAddress  string `json:"full_address"`
	}

	result := make([]propertyResponse, 0, len(buildings))
	for _, b := range buildings {
		if !b.Active {
			continue
		}
		result = append(result, propertyResponse{
			ID:           b.ID,
			Name:         b.Name,
			Address:      b.Address,
			City:         b.City,
			State:        b.State,
			Zip:          b.Zip,
			PropertyType: b.PropertyType,
			FullAddress:  b.FullAddress(),
		})
	}

	return c.JSON(http.StatusOK, result)
}

// GET /api/inspections/properties/:id — get a single property's details (building + units).
// Used when an inspector selects a property to see the available units for scoping.
func (a *App) GetPropertyDetail(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid property id")
	}

	if !a.propertyOS.IsConfigured() {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "PropertyOS not configured")
	}

	detail, err := a.propertyOS.GetBuilding(ctx, id)
	if err != nil {
		c.Logger().Errorf("PropertyOS building detail error: %v", err)
		return echo.NewHTTPError(http.StatusBadGateway, "unable to fetch property details")
	}

	return c.JSON(http.StatusOK, detail)
}
