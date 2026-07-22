package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// makeJPEG builds an in-memory JPEG of the given size.
func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// ProcessUpload must report correct dimensions without decoding pixels, and must
// not ask for a transcode on ordinary JPEG.
func TestProcessUploadReadsHeaderOnly(t *testing.T) {
	data := makeJPEG(t, 640, 480)

	p, err := ProcessUpload(data)
	if err != nil {
		t.Fatalf("ProcessUpload: %v", err)
	}
	if !p.Processed {
		t.Fatal("expected Processed=true for a valid JPEG")
	}
	if p.NeedsTranscode {
		t.Error("JPEG should not need transcoding")
	}
	if p.Width != 640 || p.Height != 480 {
		t.Errorf("dimensions = %dx%d, want 640x480", p.Width, p.Height)
	}
	// The whole point of the header probe: no decoded image is retained.
	if p.Image != nil {
		t.Error("Image should be nil when no transcode is required")
	}
}

// A non-image upload must degrade to raw storage rather than erroring.
func TestProcessUploadRejectsGarbage(t *testing.T) {
	p, err := ProcessUpload([]byte("this is not an image"))
	if err != nil {
		t.Fatalf("ProcessUpload should not error on garbage: %v", err)
	}
	if p.Processed {
		t.Error("expected Processed=false for non-image data")
	}
}

// Storing the upload verbatim must preserve every byte — this is the property
// that makes stored photos true originals.
func TestSaveRawPreservesBytesExactly(t *testing.T) {
	data := makeJPEG(t, 320, 240)
	path := filepath.Join(t.TempDir(), "sub", "orig.jpg")

	if err := SaveRaw(data, path); err != nil {
		t.Fatalf("SaveRaw: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("stored bytes differ from upload: got %d bytes, want %d", len(got), len(data))
	}
}

// The async path must still produce a correctly-bounded thumbnail.
func TestMakeThumbFromBytes(t *testing.T) {
	data := makeJPEG(t, 1200, 600)
	path := filepath.Join(t.TempDir(), "thumbs", "t.jpg")

	if err := MakeThumbFromBytes(data, path, 80); err != nil {
		t.Fatalf("MakeThumbFromBytes: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open thumb: %v", err)
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("decode thumb: %v", err)
	}
	if cfg.Width > 400 || cfg.Height > 400 {
		t.Errorf("thumb %dx%d exceeds the 400x400 bound", cfg.Width, cfg.Height)
	}
	// 1200x600 fit into 400x400 preserves aspect ratio => 400x200.
	if cfg.Width != 400 || cfg.Height != 200 {
		t.Errorf("thumb = %dx%d, want 400x200 (aspect preserved)", cfg.Width, cfg.Height)
	}
}

// Enqueue must actually render, and must not drop work when the queue overflows.
func TestThumbQueueRendersAndFallsBackInline(t *testing.T) {
	dir := t.TempDir()
	data := makeJPEG(t, 800, 800)

	q := NewThumbQueue(2, 4)
	paths := make([]string, 12)
	for i := range paths {
		paths[i] = filepath.Join(dir, string(rune('a'+i))+".jpg")
		q.Enqueue(data, paths[i], 80)
	}
	q.Shutdown() // drains outstanding work

	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("thumbnail missing for %s: %v", filepath.Base(p), err)
		}
	}
}

// An empty thumb path is a no-op, not a panic or a stray file.
func TestThumbQueueIgnoresEmptyPath(t *testing.T) {
	q := NewThumbQueue(1, 1)
	q.Enqueue(makeJPEG(t, 64, 64), "", 80)
	q.Shutdown()
}
