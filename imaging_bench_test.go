package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"path/filepath"
	"testing"

	"github.com/disintegration/imaging"
)

// bigJPEG builds a 4284x5712 (24.5 MP) JPEG — the resolution the field fleet
// started uploading on 2026-07-06.
func bigJPEG(tb testing.TB) []byte {
	tb.Helper()
	const w, h = 4284, 5712
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		tb.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// OLD: full decode + Lanczos thumb + full-res q=95 re-encode, all inline on the
// request goroutine.
func BenchmarkOldInlinePipeline(b *testing.B) {
	data := bigJPEG(b)
	dir := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
		if err != nil {
			b.Fatal(err)
		}
		thumb := imaging.Fit(img, 400, 400, imaging.Lanczos)
		if err := SaveImage(img, filepath.Join(dir, "full.jpg"), 95); err != nil {
			b.Fatal(err)
		}
		if err := SaveImage(thumb, filepath.Join(dir, "thumb.jpg"), 80); err != nil {
			b.Fatal(err)
		}
	}
}

// NEW: header probe + verbatim byte write. This is all the client now waits for.
func BenchmarkNewRequestPath(b *testing.B) {
	data := bigJPEG(b)
	dir := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := ProcessUpload(data)
		if err != nil || !p.Processed {
			b.Fatal("probe failed")
		}
		if err := SaveRaw(data, filepath.Join(dir, "full.jpg")); err != nil {
			b.Fatal(err)
		}
	}
}

// The deferred work, for reference — no longer blocks the client.
func BenchmarkDeferredThumb(b *testing.B) {
	data := bigJPEG(b)
	dir := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := MakeThumbFromBytes(data, filepath.Join(dir, "t.jpg"), 80); err != nil {
			b.Fatal(err)
		}
	}
}
