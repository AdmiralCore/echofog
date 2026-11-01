// Package nativecap is the app's own screen capture for streams: Windows.Graphics.Capture keeps
// frames on the GPU, the video engine converts them to NV12 and a hardware H.264 encoder (Media
// Foundation) compresses them; the result goes into the call as RTP, as if the webview had sent
// it. It exists because the webview's own capture copies every frame through system memory and
// holds itself to half a CPU core, which keeps a screen at 30-50 fps. Elsewhere than on Windows,
// and where any piece is missing, Available reports false and the app shares through the webview.
package nativecap

import "time"

// Source is a monitor or a window that can be shared.
type Source struct {
	ID        string `json:"id"`   // "monitor:<n>" or "window:<handle>"
	Kind      string `json:"kind"` // "monitor" | "window"
	Title     string `json:"title"`
	App       string `json:"app,omitempty"` // a window's program, e.g. "chrome.exe"
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	RefreshHz int    `json:"refreshHz,omitempty"` // a monitor's refresh rate
	Primary   bool   `json:"primary,omitempty"`
	Minimized bool   `json:"minimized,omitempty"` // a minimized window sends nothing until restored
}

// Options describe one native screen stream.
type Options struct {
	Source string // a Source ID
	// Width and Height bound the picture: the source is scaled down to fit, never up.
	Width, Height int
	FPS           int
	BitrateKbps   int
	Cursor        bool
	// Publish receives every RTP packet, paced; it must not keep the slice.
	Publish func(packet []byte)
	// MTU is the largest RTP packet to make.
	MTU int
	// OnEnd is called once, on a goroutine of its own, when the stream stops by itself (the window
	// closed, the GPU was reset).
	OnEnd func(err error)
}

// Stats are totals since the stream started, plus what it does now; the app turns totals into
// rates the same way it does for the webview's encoder.
type Stats struct {
	Captured      uint64  `json:"captured"`      // frames the capture delivered
	Encoded       uint64  `json:"encoded"`       // frames the encoder produced
	Sent          uint64  `json:"sent"`          // frames whose packets all went out
	Skipped       uint64  `json:"skipped"`       // times a frame found the encoder busy and waited for it
	Keyframes     uint64  `json:"keyframes"`     // IDR frames made
	Bytes         uint64  `json:"bytes"`         // RTP bytes sent
	Packets       uint64  `json:"packets"`       // RTP packets sent
	EncodeSeconds float64 `json:"encodeSeconds"` // summed time from a frame going in to its bits coming out
	Width         int     `json:"width"`         // encoded size
	Height        int     `json:"height"`
	SourceWidth   int     `json:"sourceWidth"` // captured size
	SourceHeight  int     `json:"sourceHeight"`
	BitrateKbps   int     `json:"bitrateKbps"` // what the encoder is set to now
	Encoder       string  `json:"encoder"`     // e.g. "NVIDIA H.264 Encoder MFT"
	Adapter       string  `json:"adapter"`
	Hardware      bool    `json:"hardware"`
	PacerDelayMs  float64 `json:"pacerDelayMs"` // how long the last frame waited in the pacer
}

// After adds the totals of an earlier capture of the same stream (a quality change restarts the
// encoder), so counters only grow and rates over the restart stay right.
func (s Stats) After(earlier Stats) Stats {
	s.Captured += earlier.Captured
	s.Encoded += earlier.Encoded
	s.Sent += earlier.Sent
	s.Skipped += earlier.Skipped
	s.Keyframes += earlier.Keyframes
	s.Bytes += earlier.Bytes
	s.Packets += earlier.Packets
	s.EncodeSeconds += earlier.EncodeSeconds
	return s
}

// minFrameGap is how long a still picture goes without a frame: a repeat of the last one keeps
// viewers from counting the wait as a freeze, at the cost of a few hundred bytes.
const minFrameGap = 100 * time.Millisecond

// frameClock decides when a picture goes to the encoder: on a steady beat at the target rate,
// with the newest picture. The capture runs at the screen's refresh rate, which is not a multiple
// of the beat, so a beat that finds nothing new does not lose its frame: the next picture goes out
// as soon as it comes. A still picture is repeated every minFrameGap.
type frameClock struct {
	interval time.Duration
	next     time.Time // the next beat
	last     time.Time // when the last frame went out
	fresh    bool      // a picture came since the last frame
	owed     bool      // a beat passed with nothing new
}

func newFrameClock(fps int, now time.Time) *frameClock {
	iv := time.Second / time.Duration(max(fps, 1))
	return &frameClock{interval: iv, next: now.Add(iv), last: now}
}

// captured notes a new picture; true means it goes out now, for a beat that was owed.
func (c *frameClock) captured() bool {
	c.fresh = true
	return c.owed
}

// beat is called at c.next; true means a frame goes out now.
func (c *frameClock) beat(now time.Time) bool {
	c.next = c.next.Add(c.interval)
	if behind := now.Sub(c.next); behind > 0 {
		c.next = c.next.Add((behind/c.interval + 1) * c.interval) // late by beats: skip them
	}
	if c.fresh || now.Sub(c.last) >= minFrameGap {
		return true
	}
	c.owed = true
	return false
}

// sent notes that a frame went out.
func (c *frameClock) sent(now time.Time) {
	c.fresh, c.owed, c.last = false, false, now
}

// outputSize fits a w x h picture into maxW x maxH without enlarging it, in even numbers as NV12
// needs.
func outputSize(w, h, maxW, maxH int) (int, int) {
	if w <= 0 || h <= 0 {
		return maxW &^ 1, maxH &^ 1
	}
	if maxW > 0 && w > maxW {
		h, w = h*maxW/w, maxW
	}
	if maxH > 0 && h > maxH {
		w, h = w*maxH/h, maxH
	}
	return max(w&^1, 2), max(h&^1, 2)
}
