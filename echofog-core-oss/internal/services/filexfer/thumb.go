package filexfer

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	_ "image/gif" // decoders for Thumbnail
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A thumbnail travels with a picture's offer, so the chat shows the picture at once, before (and
// without) the file being fetched. It is small enough to ride in the offer message itself.

const (
	thumbSide     = 192       // the longer side, in pixels
	thumbMaxBytes = 6 << 10   // the encoded JPEG, at most
	thumbMaxFile  = 60 << 20  // pictures larger than this get no thumbnail
	thumbMaxPix   = 60 << 20  // nor do pictures of more pixels than this (decoding would be costly)
	exifScan      = 128 << 10 // where the orientation tag is looked for in a JPEG
)

// Thumbnail makes the thumbnail of the picture at path: base64 JPEG and its size. ok is false for
// anything that is not a JPEG, PNG or GIF picture, or one too large to be worth it.
func Thumbnail(path string) (b64 string, w, h int, ok bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".gif":
	default:
		return "", 0, 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > thumbMaxFile {
		return "", 0, 0, false
	}
	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > thumbMaxPix {
		return "", 0, 0, false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, 0, false
	}
	head := make([]byte, exifScan)
	n, _ := io.ReadFull(f, head)
	orient := jpegOrientation(head[:n])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, 0, false
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return "", 0, 0, false
	}
	for side := thumbSide; side >= 64; side = side * 3 / 4 {
		small := orient.apply(downscale(img, side))
		for q := 72; q >= 40; q -= 8 {
			var buf bytes.Buffer
			if jpeg.Encode(&buf, small, &jpeg.Options{Quality: q}) != nil {
				return "", 0, 0, false
			}
			if buf.Len() <= thumbMaxBytes {
				b := small.Bounds()
				return base64.StdEncoding.EncodeToString(buf.Bytes()), b.Dx(), b.Dy(), true
			}
		}
	}
	return "", 0, 0, false
}

// downscale fits img into side x side, averaging the source pixels behind each one (a box filter:
// no aliasing, no dependency).
func downscale(img image.Image, side int) *image.RGBA {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	w, h := sw, sh
	if w > side || h > side {
		if w >= h {
			w, h = side, max(1, sh*side/sw)
		} else {
			w, h = max(1, sw*side/sh), side
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := b.Min.Y+y*sh/h, b.Min.Y+max((y+1)*sh/h, y*sh/h+1)
		for x := 0; x < w; x++ {
			x0, x1 := b.Min.X+x*sw/w, b.Min.X+max((x+1)*sw/w, x*sw/w+1)
			var r, g, bl, a, n uint64
			// sample at most 4x4 source pixels per output pixel: plenty for a thumbnail
			sx, sy := max(1, (x1-x0)/4), max(1, (y1-y0)/4)
			for yy := y0; yy < y1; yy += sy {
				for xx := x0; xx < x1; xx += sx {
					cr, cg, cb, ca := img.At(xx, yy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			out.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), uint8(a / n >> 8)})
		}
	}
	return out
}

// orientation is a JPEG's EXIF orientation (1 upright; 3 upside down; 6 and 8 turned).
type orientation int

func (o orientation) apply(img *image.RGBA) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var out *image.RGBA
	switch o {
	case 3:
		out = image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.SetRGBA(w-1-x, h-1-y, img.RGBAAt(x, y))
			}
		}
	case 6: // turned left: show it turned right
		out = image.NewRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.SetRGBA(h-1-y, x, img.RGBAAt(x, y))
			}
		}
	case 8:
		out = image.NewRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.SetRGBA(y, w-1-x, img.RGBAAt(x, y))
			}
		}
	default:
		return img
	}
	return out
}

// jpegOrientation reads the EXIF orientation of a JPEG from its first bytes (1 when there is none).
func jpegOrientation(b []byte) orientation {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) && b[i] == 0xFF {
		marker := b[i+1]
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if marker == 0xE1 && i+2+size <= len(b) && size >= 8 && string(b[i+4:i+10]) == "Exif\x00\x00" {
			return tiffOrientation(b[i+10 : i+2+size])
		}
		if marker == 0xDA || size < 2 {
			break
		}
		i += 2 + size
	}
	return 1
}

func tiffOrientation(t []byte) orientation {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder = binary.BigEndian
	if string(t[:2]) == "II" {
		bo = binary.LittleEndian
	}
	off := int(bo.Uint32(t[4:]))
	if off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			break
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if o := orientation(bo.Uint16(t[e+8:])); o >= 1 && o <= 8 {
				return o
			}
		}
	}
	return 1
}
