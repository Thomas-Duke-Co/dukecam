package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
)

// listPropertiesForPicker returns active buildings for the inspection + capture
// property pickers, preferring the direct DB read (which includes the staging
// Taliercio prospect buildings) and falling back to the PropertyOS API.
func (a *App) listPropertiesForPicker(ctx context.Context) ([]Building, error) {
	if a.posDB != nil {
		props, err := a.posDB.ListProperties(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Building, 0, len(props))
		for _, p := range props {
			out = append(out, Building{ID: p.ID, Name: p.Name, Address: p.Address, City: p.City, Active: true})
		}
		return out, nil
	}
	if a.propertyOS != nil && a.propertyOS.IsConfigured() {
		return a.propertyOS.ListBuildings(ctx)
	}
	return []Building{}, nil
}

// CaptureProperties returns the building list for the capture-page property picker.
// GET /api/capture/properties
func (a *App) CaptureProperties(c echo.Context) error {
	if a.posDB == nil {
		return c.JSON(http.StatusOK, []POSProperty{})
	}
	props, err := a.posDB.ListProperties(c.Request().Context())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "properties unavailable")
	}
	return c.JSON(http.StatusOK, props)
}

// CaptureUnits returns the suites for a building for the unit picker.
// GET /api/capture/units/:bid
func (a *App) CaptureUnits(c echo.Context) error {
	if a.posDB == nil {
		return c.JSON(http.StatusOK, []POSUnit{})
	}
	bid, err := strconv.Atoi(c.Param("bid"))
	if err != nil || bid <= 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "bad building id")
	}
	units, err := a.posDB.ListUnits(c.Request().Context(), bid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "units unavailable")
	}
	return c.JSON(http.StatusOK, units)
}
