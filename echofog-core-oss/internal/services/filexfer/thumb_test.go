package filexfer

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// A picture gets a small JPEG thumbnail that keeps its proportions; anything else gets none.
func TestThumbnail(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1200; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	pic := filepath.Join(dir, "photo.png")
	if err := os.WriteFile(pic, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	b64, w, h, ok := Thumbnail(pic)
	if !ok || w != 192 || h != 128 {
		t.Fatalf("thumbnail: ok %v, %dx%d", ok, w, h)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) > thumbMaxBytes {
		t.Fatalf("encoded %d bytes, err %v", len(raw), err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}

	doc := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(doc, []byte("hello"), 0o600)
	if _, _, _, ok := Thumbnail(doc); ok {
		t.Fatal("a text file got a thumbnail")
	}
	fake := filepath.Join(dir, "broken.jpg")
	_ = os.WriteFile(fake, []byte("not a picture"), 0o600)
	if _, _, _, ok := Thumbnail(fake); ok {
		t.Fatal("a broken picture got a thumbnail")
	}
}

// A phone photo stored sideways (EXIF orientation 6) comes out upright.
func TestThumbnailOrientation(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	if o := orientation(6).apply(src).Bounds(); o.Dx() != 2 || o.Dy() != 4 {
		t.Fatalf("turned: %v", o)
	}
	// a minimal EXIF segment saying orientation 6, big-endian
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, 0, 0, 0, 0}
	seg := append([]byte("Exif\x00\x00"), tiff...)
	jpg := append([]byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)}, seg...)
	if o := jpegOrientation(jpg); o != 6 {
		t.Fatalf("orientation %d", o)
	}
}
