package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/disintegration/imaging"
	"github.com/rwcarlsen/goexif/exif"
)

// EXIFData holds extracted GPS and timestamp from image EXIF.
type EXIFData struct {
	Lat     *float64
	Lng     *float64
	TakenAt *time.Time
}

// ProcessedImage holds the result of image processing.
type ProcessedImage struct {
	Image     image.Image
	Thumb     image.Image
	EXIF      EXIFData
	Width     int
	Height    int
	Processed bool // false if image couldn't be decoded (e.g., HEIC)
}

// ExtractEXIF reads GPS and DateTime from EXIF data.
func ExtractEXIF(data []byte) EXIFData {
	result := EXIFData{}

	x, err := exif.Decode(bytes.NewReader(data))
	if err != nil {
		return result
	}

	// DateTime
	if dt, err := x.DateTime(); err == nil {
		result.TakenAt = &dt
	}

	// GPS
	if lat, lng, err := x.LatLong(); err == nil {
		result.Lat = &lat
		result.Lng = &lng
	}

	return result
}

// ProcessUpload decodes, orients, and creates a thumbnail from uploaded image data.
func ProcessUpload(data []byte) (*ProcessedImage, error) {
	reader := bytes.NewReader(data)

	// Decode with auto-orientation
	img, err := imaging.Decode(reader, imaging.AutoOrientation(true))
	if err != nil {
		// Can't decode — store raw, no processing
		return &ProcessedImage{Processed: false}, nil
	}

	// Extract EXIF from original bytes (before any transforms)
	exifData := ExtractEXIF(data)

	// Create thumbnail (400x400 max, preserving aspect ratio)
	thumb := imaging.Fit(img, 400, 400, imaging.Lanczos)

	return &ProcessedImage{
		Image:     img,
		Thumb:     thumb,
		EXIF:      exifData,
		Width:     img.Bounds().Dx(),
		Height:    img.Bounds().Dy(),
		Processed: true,
	}, nil
}

// SaveImage writes an image to disk as JPEG or PNG based on extension.
func SaveImage(img image.Image, path string, quality int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".png" {
		return png.Encode(f, img)
	}
	return jpeg.Encode(f, img, &jpeg.Options{Quality: quality})
}

// SaveRaw writes raw bytes to disk (for formats we can't process).
func SaveRaw(data []byte, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// FindFile locates a file in a date-organized directory tree.
// Path pattern: {base}/{slug}/{YYYY}/{MM}/{DD}/{filename}
func FindFile(basePath, slug, filename string) (string, error) {
	pattern := filepath.Join(basePath, slug, "*", "*", "*", filename)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("file not found: %s/%s", slug, filename)
	}
	return matches[0], nil
}

// RotateFile reads an image from disk, rotates it, saves it back, and regenerates the thumbnail.
// direction: "cw" for 90° clockwise, "ccw" for 90° counter-clockwise, "180" for 180°.
func RotateFile(photoPath, thumbPath, direction string) (int, int, error) {
	img, err := imaging.Open(photoPath)
	if err != nil {
		return 0, 0, fmt.Errorf("open image: %w", err)
	}

	var rotated *image.NRGBA
	switch direction {
	case "cw":
		rotated = imaging.Rotate270(img) // 270° CCW = 90° CW
	case "ccw":
		rotated = imaging.Rotate90(img) // 90° CCW
	case "180":
		rotated = imaging.Rotate180(img)
	default:
		return 0, 0, fmt.Errorf("invalid direction: %s", direction)
	}

	ext := strings.ToLower(filepath.Ext(photoPath))
	quality := 85
	if ext == ".png" {
		quality = 0
	}

	if err := SaveImage(rotated, photoPath, quality); err != nil {
		return 0, 0, fmt.Errorf("save rotated: %w", err)
	}

	// Regenerate thumbnail
	if thumbPath != "" {
		thumb := imaging.Fit(rotated, 400, 400, imaging.Lanczos)
		thumbQuality := 80
		if ext == ".png" {
			thumbQuality = 0
		}
		if err := SaveImage(thumb, thumbPath, thumbQuality); err != nil {
			// Non-fatal
			fmt.Printf("regenerate thumb error: %v\n", err)
		}
	}

	return rotated.Bounds().Dx(), rotated.Bounds().Dy(), nil
}

// EncodeToBytes encodes an image to a byte slice (for QR generation etc).
func EncodeToBytes(img image.Image, format string) ([]byte, error) {
	var buf bytes.Buffer
	var w io.Writer = &buf

	switch format {
	case "png":
		if err := png.Encode(w, img); err != nil {
			return nil, err
		}
	default:
		if err := jpeg.Encode(w, img, &jpeg.Options{Quality: 85}); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
