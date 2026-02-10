package main

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// ServePhoto serves a full-size photo from the storage directory.
// Uses glob to find the file in the date-organized tree.
func (a *App) ServePhoto(c echo.Context) error {
	slug := c.Param("slug")
	filename := c.Param("filename")

	path, err := FindFile(a.config.StoragePath, slug, filename)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	return c.File(path)
}

// ServeThumb serves a thumbnail from the thumbnail directory.
func (a *App) ServeThumb(c echo.Context) error {
	slug := c.Param("slug")
	filename := c.Param("filename")

	path, err := FindFile(a.config.ThumbPath, slug, filename)
	if err != nil {
		// Fall back to full photo if no thumbnail
		path, err = FindFile(a.config.StoragePath, slug, filename)
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound)
		}
	}

	return c.File(path)
}
