//go:build windows

package nativecap

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"sync"
	"time"
	"unsafe"
)

// Live thumbnails for the picker: while it is open every source it shows is captured twice a
// second, scaled down on the GPU and handed to the interface as a JPEG, so people see which
// window is which instead of guessing from titles. They must not get in the way of a stream the
// computer plays meanwhile: a laptop decoding a 120 fps stream fell to 0-17 fps while thumbnails
// read their pictures back the plain way, which waits for the GPU each time. A picture is copied
// to a staging texture on one beat and read on the next, if the GPU is done with it by then.

const (
	previewW, previewH = 320, 180
	previewEvery       = 500 * time.Millisecond
	previewRetry       = 2 * time.Second // a source that would not open is tried again this late
	previewIdle        = 5 * time.Second // nobody asked for pictures this long: the captures close
	maxPreviews        = 8               // each capture holds a full-size frame on the GPU

	d3d11MapFlagDoNotWait = 0x100000
	dxgiErrorStillDrawing = hresult(-2005270518) // 0x887A000A DXGI_ERROR_WAS_STILL_DRAWING
)

type previewer struct {
	mu      sync.Mutex
	want    []string
	images  map[string]string // source ID -> data URL
	asked   time.Time
	running bool
}

var previews = previewer{images: map[string]string{}}

// Previews sets the sources whose thumbnails the picker shows and returns the newest ones as data
// URLs by source ID. The captures start with the first call and close by themselves when an empty
// list comes or nobody asks for a while.
func Previews(ids []string) map[string]string {
	p := &previews
	p.mu.Lock()
	defer p.mu.Unlock()
	p.want = append(p.want[:0], ids...)
	if len(p.want) > maxPreviews {
		p.want = p.want[:maxPreviews]
	}
	p.asked = time.Now()
	if len(ids) == 0 {
		p.images = map[string]string{}
		return nil
	}
	if !p.running {
		p.running = true
		go p.run()
	}
	out := make(map[string]string, len(p.images))
	for _, id := range p.want {
		if img, ok := p.images[id]; ok {
			out[id] = img
		}
	}
	return out
}

type previewSource struct {
	staging   unsafe.Pointer // a CPU-readable copy of tex, read a beat after it was made
	copied    bool           // staging holds a picture not read yet
	cap       *wgcCapture
	conv      *converter
	tex, view unsafe.Pointer
	w, h      uint32
	failedAt  time.Time
}

func (s *previewSource) close() {
	if s.conv != nil {
		s.conv.close()
	}
	release(s.view)
	release(s.tex)
	release(s.staging)
	if s.cap != nil {
		s.cap.close()
	}
}

func (p *previewer) stop() {
	p.mu.Lock()
	p.running = false
	p.images = map[string]string{}
	p.mu.Unlock()
}

func (p *previewer) run() {
	undo, err := mtaThread()
	if err != nil {
		p.stop()
		return
	}
	defer undo()
	d, err := newDevice()
	if err != nil {
		p.stop()
		return
	}
	defer d.close()
	srcs := map[string]*previewSource{}
	defer func() {
		for _, s := range srcs {
			s.close()
		}
	}()
	tick := time.NewTicker(previewEvery)
	defer tick.Stop()
	for range tick.C {
		p.mu.Lock()
		want := append([]string(nil), p.want...)
		idle := time.Since(p.asked) > previewIdle || len(want) == 0
		if idle {
			p.running = false
			p.images = map[string]string{}
		}
		p.mu.Unlock()
		if idle {
			return
		}
		keep := map[string]bool{}
		for _, id := range want {
			keep[id] = true
			s := srcs[id]
			if s == nil {
				s = &previewSource{}
				srcs[id] = s
			}
			if s.cap == nil {
				if time.Since(s.failedAt) < previewRetry {
					continue
				}
				if err := s.open(d, id); err != nil {
					s.close()
					*s = previewSource{failedAt: time.Now()}
					continue
				}
			}
			if img, ok := s.grab(d); ok {
				p.mu.Lock()
				p.images[id] = img
				p.mu.Unlock()
			}
		}
		for id, s := range srcs {
			if !keep[id] {
				s.close()
				delete(srcs, id)
				p.mu.Lock()
				delete(p.images, id)
				p.mu.Unlock()
			}
		}
	}
}

func (s *previewSource) open(d *device, id string) error {
	tg, err := target(id)
	if err != nil {
		return err
	}
	if s.cap, err = newWGCCapture(d, tg, false, previewEvery, 1); err != nil {
		return err
	}
	return s.remake(d)
}

// remake sizes the thumbnail to the picture's shape and makes its converter.
func (s *previewSource) remake(d *device) error {
	if s.conv != nil {
		s.conv.close()
	}
	release(s.view)
	release(s.tex)
	release(s.staging)
	s.conv, s.view, s.tex, s.staging, s.copied = nil, nil, nil, nil, false
	w, h := outputSize(int(s.cap.contentW), int(s.cap.contentH), previewW, previewH)
	s.w, s.h = uint32(w), uint32(h)
	var err error
	if s.conv, err = newConverter(d, uint32(s.cap.poolW), uint32(s.cap.poolH), s.w, s.h, true); err != nil {
		return err
	}
	if s.tex, err = d.texture(texture2DDesc{Width: s.w, Height: s.h, Format: dxgiFormatB8G8R8A8, BindFlags: d3d11BindRenderTarget}); err != nil {
		return err
	}
	if s.staging, err = d.texture(texture2DDesc{Width: s.w, Height: s.h, Format: dxgiFormatB8G8R8A8, Usage: d3d11UsageStaging, CPUAccessFlags: d3d11CPUAccessRead}); err != nil {
		return err
	}
	s.view, err = s.conv.outputView(s.tex)
	return err
}

// grab reads the picture the last beat copied, if the GPU is done with it, and copies the newest
// frame for the next beat. It returns a JPEG data URL when it read one.
func (s *previewSource) grab(d *device) (string, bool) {
	img := s.read(d)
	got, resized, err := s.cap.next()
	switch {
	case err != nil:
	case resized:
		_ = s.remake(d) // the next frame has the new size
		img = nil
	case got && !s.copied:
		if s.conv.convert(s.cap.latest, s.cap.contentW, s.cap.contentH, s.view) == nil {
			d.copyResource(s.staging, s.tex)
			s.copied = true
		}
	}
	if img == nil {
		return "", false
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 72}); err != nil {
		return "", false
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), true
}

// read maps the staging copy without waiting: nil when there is none or the GPU is still on it.
func (s *previewSource) read(d *device) *image.RGBA {
	if !s.copied || s.staging == nil {
		return nil
	}
	var mapped struct {
		Data       unsafe.Pointer
		RowPitch   uint32
		DepthPitch uint32
	}
	hr := comCall(d.ctx, ctxMap, uintptr(s.staging), 0, 1 /* D3D11_MAP_READ */, d3d11MapFlagDoNotWait, uintptr(unsafe.Pointer(&mapped)))
	if hr == dxgiErrorStillDrawing {
		return nil // next beat
	}
	s.copied = false
	if hr < 0 {
		return nil
	}
	defer comCall(d.ctx, ctxUnmap, uintptr(s.staging), 0)
	w, h := int(s.w), int(s.h)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		row := unsafe.Slice((*byte)(unsafe.Add(mapped.Data, uintptr(y)*uintptr(mapped.RowPitch))), w*4)
		out := img.Pix[y*img.Stride:]
		for x := 0; x < w*4; x += 4 { // BGRA -> RGBA
			out[x], out[x+1], out[x+2], out[x+3] = row[x+2], row[x+1], row[x], 255
		}
	}
	return img
}
