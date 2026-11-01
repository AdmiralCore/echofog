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

// Live thumbnails for the picker: while it is open every source it shows is captured a few times
// a second, scaled down on the GPU and handed to the interface as a JPEG, so people see which
// window is which instead of guessing from titles.

const (
	previewW, previewH = 320, 180
	previewEvery       = 250 * time.Millisecond
	previewRetry       = 2 * time.Second // a source that would not open is tried again this late
	previewIdle        = 5 * time.Second // nobody asked for pictures this long: the captures close
	maxPreviews        = 12              // each capture holds a full-size frame on the GPU
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
	s.conv, s.view, s.tex = nil, nil, nil
	w, h := outputSize(int(s.cap.contentW), int(s.cap.contentH), previewW, previewH)
	s.w, s.h = uint32(w), uint32(h)
	var err error
	if s.conv, err = newConverter(d, uint32(s.cap.poolW), uint32(s.cap.poolH), s.w, s.h, true); err != nil {
		return err
	}
	if s.tex, err = d.texture(texture2DDesc{Width: s.w, Height: s.h, Format: dxgiFormatB8G8R8A8, BindFlags: d3d11BindRenderTarget}); err != nil {
		return err
	}
	s.view, err = s.conv.outputView(s.tex)
	return err
}

// grab turns the newest frame, if there is one, into a JPEG data URL.
func (s *previewSource) grab(d *device) (string, bool) {
	got, resized, err := s.cap.next()
	if err != nil || (!got && !resized) {
		return "", false
	}
	if resized {
		if err := s.remake(d); err != nil {
			return "", false
		}
		return "", false // the next frame has the new size
	}
	if err := s.conv.convert(s.cap.latest, s.cap.contentW, s.cap.contentH, s.view); err != nil {
		return "", false
	}
	px, w, h, err := d.readPixels(s.tex)
	if err != nil {
		return "", false
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i+3 < len(px); i += 4 { // BGRA -> RGBA
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = px[i+2], px[i+1], px[i], 255
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 72}); err != nil {
		return "", false
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), true
}
