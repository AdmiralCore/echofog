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

	enc   *mfEncoder
	pacer *pacer
	pk    *packetizer
	tsAt  uint32 // RTP timestamp of pts 0

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
	if s.enc != nil {
		s.enc.setBitrate(uint32(min(bps, 1<<31)))
	}
	if s.pacer != nil {
		s.pacer.bitrate.Store(bps)
	}
}

// RequestKeyframe makes the next frame an IDR (a viewer joined or lost packets).
func (s *Stream) RequestKeyframe() { s.wantKey.Store(true) }

// Stats returns the totals so far.
func (s *Stream) Stats() Stats {
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	if p := s.pacer; p != nil {
		st.Sent, st.Packets, st.Bytes = p.frames.Load(), p.packets.Load(), p.sentBytes.Load()
		st.PacerDelayMs = float64(p.lastDelay.Load()) / 1e6
	}
	if e := s.enc; e != nil {
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
	c, err := newWGCCapture(d, tg, s.o.Cursor, time.Millisecond)
	if err != nil {
		endErr = fmt.Errorf("capture: %w", err)
		return
	}
	defer c.close()
	outW, outH := outputSize(int(c.contentW), int(c.contentH), s.o.Width, s.o.Height)
	conv, err := newConverter(d, uint32(c.poolW), uint32(c.poolH), uint32(outW), uint32(outH))
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
	enc, err := newMFEncoder(d, outW, outH, s.o.FPS, s.o.BitrateKbps, s.onFrame)
	if err != nil {
		endErr = fmt.Errorf("encoder: %w", err)
		return
	}
	s.enc = enc
	defer func() {
		enc.drain(time.Second)
		enc.close()
	}()
	s.pacer.start()
	defer s.pacer.close()
	s.mu.Lock()
	s.st.Width, s.st.Height = outW, outH
	s.st.SourceWidth, s.st.SourceHeight = int(c.contentW), int(c.contentH)
	s.st.Encoder, s.st.Adapter, s.st.Hardware = enc.name, d.adapterName, true
	s.mu.Unlock()
	started = true
	ready <- nil

	start := time.Now()
	clock := newFrameClock(s.o.FPS, start)
	beat := time.NewTimer(time.Until(clock.next))
	defer beat.Stop()
	check := time.NewTicker(time.Second)
	defer check.Stop()
	send := func(now time.Time) {
		if err := s.send(d, conv, c, now.Sub(start)); err != nil {
			endErr = err
		}
		clock.sent(now)
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
				if conv, err = newConverter(d, uint32(c.poolW), uint32(c.poolH), uint32(outW), uint32(outH)); err != nil {
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
		case <-check.C:
			switch {
			case c.gone():
				endErr = errors.New("the shared window was closed")
			case d.removed() != nil:
				endErr = fmt.Errorf("the graphics driver reset: %w", d.removed())
			case enc.failed() != nil:
				endErr = fmt.Errorf("encoder: %w", enc.failed())
			}
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

// send converts the newest picture into a free NV12 frame and hands it to the encoder; when the
// encoder is still busy the frame is skipped, and the picture goes with the next beat.
func (s *Stream) send(d *device, conv *converter, c *wgcCapture, pts time.Duration) error {
	s.mu.Lock()
	slot := -1
	for i := range s.slots {
		if !s.slots[i].busy {
			slot = i
			break
		}
	}
	if slot < 0 || !s.enc.ready() {
		s.st.Skipped++
		s.mu.Unlock()
		return nil
	}
	s.slots[slot].busy, s.slots[slot].pts = true, pts
	sl := s.slots[slot]
	s.mu.Unlock()
	if err := conv.convert(c.latest, c.contentW, c.contentH, sl.view); err != nil {
		return fmt.Errorf("video processor: %w", err)
	}
	d.flush()
	key := s.wantKey.Swap(false)
	if err := s.enc.encode(sl.tex, pts, key); err != nil {
		s.mu.Lock()
		s.slots[slot].busy = false
		s.mu.Unlock()
		return fmt.Errorf("encoder: %w", err)
	}
	return nil
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
	s.pacer.push(s.pk.packets(f.data, ts))
}
