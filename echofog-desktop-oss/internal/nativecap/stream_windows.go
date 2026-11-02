//go:build windows

package nativecap

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// nv12Slots is how many converted frames may be with the encoder at once; a frame that finds none
// free is skipped (the encoder is behind) rather than overwriting one being encoded.
const nv12Slots = 4

var (
	availOnce sync.Once
	availErr  error
)

// Available reports why this computer cannot stream its screen natively, or nil when it can:
// Windows.Graphics.Capture and a hardware H.264 encoder are both there. Checked once.
func Available() error {
	availOnce.Do(func() {
		ready := make(chan error, 1)
		go func() {
			undo, err := mtaThread()
			if err != nil {
				ready <- err
				return
			}
			defer undo()
			if !captureSupported() {
				ready <- errors.New("Windows.Graphics.Capture is not available (Windows 10 1903 or newer is needed)")
				return
			}
			if err := mfStartup(); err != nil {
				ready <- err
				return
			}
			_, free, err := hardwareEncoders()
			if err != nil {
				ready <- err
				return
			}
			free()
			ready <- nil
		}()
		availErr = <-ready
	})
	return availErr
}

// Stream is one running native screen stream.
type Stream struct {
	o     Options
	stop  chan struct{}
	done  chan struct{}
	ended atomic.Bool // stopped by Stop, not by itself

	wantKey atomic.Bool
	bitrate atomic.Uint64
	viewers atomic.Pointer[[]int] // the refresh rates of the viewers' screens

	enc     atomic.Pointer[mfEncoder] // a change of level replaces it
	encMu   sync.Mutex                // keeps a bitrate change off an encoder being closed
	pacer   *pacer
	pk      *packetizer
	tsAt    uint32        // RTP timestamp of pts 0
	base    time.Duration // qpcNow at the start: pts 0
	lastPts time.Duration

	mu    sync.Mutex
	slots [nv12Slots]nv12Slot
	st    Stats
}

type nv12Slot struct {
	tex, view unsafe.Pointer
	busy      bool
	pts       time.Duration
}

// Start opens the source and the encoder and begins sending; it returns once the first part
// worked or failed, so the caller can fall back to the webview at once.
func Start(o Options) (*Stream, error) {
	if o.FPS <= 0 || o.BitrateKbps <= 0 || o.Publish == nil {
		return nil, errors.New("native stream: fps, bitrate and a publisher are needed")
	}
	if o.MTU <= 0 {
		o.MTU = 1100
	}
	s := &Stream{o: o, stop: make(chan struct{}), done: make(chan struct{}), tsAt: rand.Uint32()}
	s.bitrate.Store(uint64(o.BitrateKbps) * 1000)
	ready := make(chan error, 1)
	go s.run(ready)
	if err := <-ready; err != nil {
		<-s.done
		return nil, err
	}
	return s, nil
}

// SetBitrate is the rate control's cap for the encoder, in bits per second.
func (s *Stream) SetBitrate(bps uint64) {
	if bps == 0 {
		return
	}
	s.bitrate.Store(bps)
	s.encMu.Lock()
	if e := s.enc.Load(); e != nil {
		e.setBitrate(uint32(min(bps, 1<<31)))
	}
	s.encMu.Unlock()
	if s.pacer != nil {
		s.pacer.bitrate.Store(bps)
	}
}

// RequestKeyframe makes the next frame an IDR (a viewer joined or lost packets).
func (s *Stream) RequestKeyframe() { s.wantKey.Store(true) }

// SetViewerRates gives the refresh rates of the viewers' screens (0: not known), which the frame
// rate follows (pickFPS).
func (s *Stream) SetViewerRates(hz []int) {
	hz = append([]int(nil), hz...)
	s.viewers.Store(&hz)
}

func (s *Stream) viewerRates() []int {
	if p := s.viewers.Load(); p != nil {
		return *p
	}
	return nil
}

func (s *Stream) closeEncoder(e *mfEncoder) {
	s.encMu.Lock()
	e.close()
	s.encMu.Unlock()
}

// Stats returns the totals so far.
func (s *Stream) Stats() Stats {
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	if p := s.pacer; p != nil {
		st.Sent, st.Packets, st.Bytes = p.frames.Load(), p.packets.Load(), p.sentBytes.Load()
		st.PacerDelayMs = float64(p.lastDelay.Load()) / 1e6
	}
	if e := s.enc.Load(); e != nil {
		st.BitrateKbps = int(e.bitrate.Load() / 1000)
	}
	return st
}

// Stop ends the stream and waits until everything is released.
func (s *Stream) Stop() {
	if s.ended.CompareAndSwap(false, true) {
		close(s.stop)
	}
	<-s.done
}

// Done is closed once the stream ended, by Stop or by itself.
func (s *Stream) Done() <-chan struct{} { return s.done }

func (s *Stream) run(ready chan<- error) {
	defer close(s.done)
	started := false
	var endErr error
	defer func() {
		if !started {
			ready <- endErr
			return
		}
		if !s.ended.Swap(true) && s.o.OnEnd != nil {
			go s.o.OnEnd(endErr) // it may stop or start streams, which wait for this one to end
		}
	}()
	undo, err := mtaThread()
	if err != nil {
		endErr = err
		return
	}
	defer undo()

	d, err := newDevice()
	if err != nil {
		endErr = fmt.Errorf("Direct3D 11: %w", err)
		return
	}
	defer d.close()
	tg, err := target(s.o.Source)
	if err != nil {
		endErr = err
		return
	}
	c, err := newWGCCapture(d, tg, s.o.Cursor, time.Millisecond, poolBuffers)
	if err != nil {
		endErr = fmt.Errorf("capture: %w", err)
		return
	}
	defer c.close()
	top := Level{Width: s.o.Width, Height: s.o.Height, FPS: s.o.FPS, Kbps: s.o.BitrateKbps}
	lad := newLadder(top, s.o.Levels)
	sourceHz := refreshRate(tg)
	lad.fps = pickFPS(top.FPS, sourceHz, s.viewerRates())
	lv := lad.start(int(s.bitrate.Load()/1000), time.Now())
	outW, outH := outputSize(int(c.contentW), int(c.contentH), lv.Width, lv.Height)
	fps := lv.FPS
	conv, err := newConverter(d, uint32(c.poolW), uint32(c.poolH), uint32(outW), uint32(outH), false)
	if err != nil {
		endErr = fmt.Errorf("video processor: %w", err)
		return
	}
	defer func() { conv.close() }()
	if err := s.makeSlots(d, conv, outW, outH); err != nil {
		endErr = err
		return
	}
	defer s.freeSlots()

	s.pk = newPacketizer(s.o.MTU)
	s.pacer = newPacer(s.o.Publish, s.bitrate.Load())
	enc, err := newMFEncoder(d, outW, outH, fps, int(s.bitrate.Load()/1000), encoderQuality, s.temporalLayers(fps), s.onFrame)
	if err != nil {
		endErr = fmt.Errorf("encoder: %w", err)
		return
	}
	s.enc.Store(enc)
	defer func() {
		e := s.enc.Load()
		e.drain(time.Second)
		s.closeEncoder(e)
	}()
	s.pacer.start()
	defer s.pacer.close()
	s.mu.Lock()
	s.st.Width, s.st.Height, s.st.FPS, s.st.Lowered = outW, outH, fps, lad.lowered()
	s.st.SourceWidth, s.st.SourceHeight = int(c.contentW), int(c.contentH)
	s.st.Encoder, s.st.Adapter, s.st.Hardware, s.st.TemporalLayers = enc.name, d.adapterName, true, enc.layers
	s.mu.Unlock()
	started = true
	ready <- nil

	start := time.Now()
	s.base = qpcNow()
	clock := newFrameClock(fps, start)
	beat := time.NewTimer(time.Until(clock.next))
	defer beat.Stop()
	check := time.NewTicker(time.Second)
	defer check.Stop()
	// a frame that finds the encoder busy waits for it to ask for the next one
	waiting := false
	// change moves the stream to another level: a new encoder (size and frame rate are fixed for
	// its life) and, for a new size, new NV12 frames. The old encoder lets out what it holds first,
	// so the frames keep their order; the new one starts with a keyframe.
	change := func(l Level, why string) error {
		w, h := outputSize(int(c.contentW), int(c.contentH), l.Width, l.Height)
		if w == outW && h == outH && l.FPS == fps {
			return nil // the picture is smaller than both levels
		}
		took := time.Now()
		old := s.enc.Load()
		old.drain(200 * time.Millisecond)
		s.closeEncoder(old)
		if w != outW || h != outH {
			s.freeSlots()
			conv.close()
			var err error
			if conv, err = newConverter(d, uint32(c.poolW), uint32(c.poolH), uint32(w), uint32(h), false); err != nil {
				return fmt.Errorf("video processor: %w", err)
			}
			if err := s.makeSlots(d, conv, w, h); err != nil {
				return err
			}
		} else {
			s.mu.Lock()
			for i := range s.slots {
				s.slots[i].busy = false // what the old encoder held is let go
			}
			s.mu.Unlock()
		}
		e, err := newMFEncoder(d, w, h, l.FPS, int(s.bitrate.Load()/1000), encoderQuality, s.temporalLayers(l.FPS), s.onFrame)
		if err != nil {
			return fmt.Errorf("encoder: %w", err)
		}
		s.enc.Store(e)
		outW, outH, fps, waiting = w, h, l.FPS, false
		clock = newFrameClock(fps, time.Now())
		beat.Reset(time.Until(clock.next))
		s.mu.Lock()
		s.st.Width, s.st.Height, s.st.FPS, s.st.TemporalLayers = outW, outH, fps, e.layers
		s.mu.Unlock()
		if s.o.OnLevel != nil {
			s.o.OnLevel(Level{Width: w, Height: h, FPS: fps, Kbps: l.Kbps}, why+", "+time.Since(took).Round(time.Millisecond).String())
		}
		return nil
	}
	send := func(now time.Time) {
		sent, err := s.send(d, conv, c)
		if err != nil {
			endErr = err
		}
		waiting = !sent
		if sent {
			clock.sent(now)
		}
	}
	for endErr == nil {
		select {
		case <-s.stop:
			return
		case <-c.arrived():
			got, resized, err := c.next()
			if err != nil {
				endErr = fmt.Errorf("capture: %w", err)
				break
			}
			if resized {
				// the window changed size: the picture keeps the stream's size, scaled to fit
				conv.close()
				if conv, err = newConverter(d, uint32(c.poolW), uint32(c.poolH), uint32(outW), uint32(outH), false); err != nil {
					endErr = fmt.Errorf("video processor: %w", err)
					break
				}
				if err := s.remakeViews(conv); err != nil {
					endErr = err
					break
				}
				s.mu.Lock()
				s.st.SourceWidth, s.st.SourceHeight = int(c.contentW), int(c.contentH)
				s.mu.Unlock()
			}
			if got {
				s.mu.Lock()
				s.st.Captured++
				s.mu.Unlock()
				if clock.captured() {
					send(time.Now())
				}
			}
		case now := <-beat.C:
			if clock.beat(now) || s.wantKey.Load() {
				send(now)
			}
			beat.Reset(time.Until(clock.next))
		case <-s.enc.Load().asked:
			if waiting {
				send(time.Now())
			}
		case now := <-check.C:
			switch {
			case c.gone():
				endErr = errors.New("the shared window was closed")
			case d.removed() != nil:
				endErr = fmt.Errorf("the graphics driver reset: %w", d.removed())
			case s.enc.Load().failed() != nil:
				endErr = fmt.Errorf("encoder: %w", s.enc.Load().failed())
			}
			if endErr != nil {
				break
			}
			if hz := refreshRate(tg); hz > 0 {
				sourceHz = hz // the monitor's mode changed, or the window moved to another one
			}
			lad.fps = pickFPS(top.FPS, sourceHz, s.viewerRates())
			if l, why, ok := lad.next(int(s.bitrate.Load()/1000), now); ok {
				if err := change(l, why); err != nil {
					endErr = err
				}
			}
			s.mu.Lock()
			s.st.Lowered = lad.lowered()
			s.mu.Unlock()
		}
	}
}

func (s *Stream) makeSlots(d *device, conv *converter, w, h int) error {
	for i := range s.slots {
		desc := texture2DDesc{Width: uint32(w), Height: uint32(h), Format: dxgiFormatNV12, BindFlags: d3d11BindRenderTarget | d3d11BindVideoEncoder}
		tex, err := d.texture(desc)
		if err != nil {
			desc.BindFlags = d3d11BindRenderTarget // older drivers do not know the encoder flag
			if tex, err = d.texture(desc); err != nil {
				return fmt.Errorf("NV12 frame: %w", err)
			}
		}
		s.slots[i].tex = tex
	}
	return s.remakeViews(conv)
}

func (s *Stream) remakeViews(conv *converter) error {
	for i := range s.slots {
		view, err := conv.outputView(s.slots[i].tex)
		if err != nil {
			return fmt.Errorf("NV12 frame: %w", err)
		}
		release(s.slots[i].view)
		s.slots[i].view = view
	}
	return nil
}

func (s *Stream) freeSlots() {
	for i := range s.slots {
		release(s.slots[i].view)
		release(s.slots[i].tex)
		s.slots[i] = nv12Slot{}
	}
}

// ptsOf is the time of the frame that goes out now: when its picture was drawn, on the capture's
// clock, so a viewer plays frames at the pace the screen showed them. The time a frame went to the
// encoder strays from that by a few milliseconds (the beat, waits for the encoder), which at 144
// fps is most of a frame: the viewer's player took such frames for late, threw ~15 of them away
// every few seconds and asked for a keyframe. A repeat of a still picture takes the time it goes
// out. Always after the last one.
func (s *Stream) ptsOf(c *wgcCapture) time.Duration {
	at := qpcNow()
	if c.latestNew {
		at = c.latestAt
	}
	return max(at-s.base, s.lastPts+100*time.Microsecond)
}

// send converts the newest picture into a free NV12 frame and hands it to the encoder. It reports
// false when the encoder is still busy: the picture goes as soon as the encoder asks for one.
func (s *Stream) send(d *device, conv *converter, c *wgcCapture) (bool, error) {
	s.mu.Lock()
	slot := -1
	for i := range s.slots {
		if !s.slots[i].busy {
			slot = i
			break
		}
	}
	if slot < 0 || !s.enc.Load().ready() {
		s.st.Skipped++
		s.mu.Unlock()
		return false, nil
	}
	pts := s.ptsOf(c)
	s.slots[slot].busy, s.slots[slot].pts = true, pts
	sl := s.slots[slot]
	s.mu.Unlock()
	if err := conv.convert(c.latest, c.contentW, c.contentH, sl.view); err != nil {
		return false, fmt.Errorf("video processor: %w", err)
	}
	d.flush()
	key := s.wantKey.Swap(false)
	if err := s.enc.Load().encode(sl.tex, pts, key); err != nil {
		s.mu.Lock()
		s.slots[slot].busy = false
		s.mu.Unlock()
		return false, fmt.Errorf("encoder: %w", err)
	}
	s.lastPts, c.latestNew = pts, false
	return true, nil
}

// temporalLayers is how many temporal layers an encoder at fps is asked for: Options.TemporalLayers,
// or by the frame rate (temporalLayersFor).
func (s *Stream) temporalLayers(fps int) int {
	if s.o.TemporalLayers > 0 {
		return s.o.TemporalLayers
	}
	return temporalLayersFor(fps)
}

// onFrame runs on the encoder's goroutine for every frame that comes out.
func (s *Stream) onFrame(f encodedFrame) {
	s.mu.Lock()
	for i := range s.slots {
		if s.slots[i].busy && s.slots[i].pts <= f.pts {
			s.slots[i].busy = false
		}
	}
	s.st.Encoded++
	s.st.EncodeSeconds += f.took.Seconds()
	if f.keyframe {
		s.st.Keyframes++
	}
	s.mu.Unlock()
	ts := s.tsAt + uint32(f.pts*90000/time.Second)
	s.pk.layered = f.layered
	s.pacer.push(s.pk.packets(f.data, ts, f.layer, f.keyframe))
}
