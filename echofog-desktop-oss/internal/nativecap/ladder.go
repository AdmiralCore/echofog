package nativecap

import (
	"math"
	"time"
)

// Level is a size and frame rate a stream can go out at, with the bitrate it needs to look right
// there: one of the call's quality presets.
type Level struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	FPS    int `json:"fps"`
	Kbps   int `json:"kbps"`
}

// The automatic quality. The rate control lowers the bitrate when the path backs up, but a stream
// kept at 1080p144 on the 12 Mbit/s a link carries gets half the bits per pixel a 720p30 stream
// gets, and looks worse than it. So the stream goes down the ladder of the presets under the
// chosen one when the rate falls well short of what its level needs, to the heaviest level the
// rate covers, and climbs back only once the rate has covered a heavier level for a while: each
// change restarts the encoder and costs a keyframe. A climb that runs back into trouble doubles the
// wait before the next one.
//
// The frame rate follows the screens: a frame rate a screen does not divide evenly is shown in
// uneven steps (144 fps on 60 Hz: 2 or 3 frames apart), which looks rougher than a lower even
// rate. The ladder keeps to the frame rate pickFPS chose, and 60 and 30 below it.
const (
	ladderLeave    = 0.6              // under this share of its level's bitrate, the stream leaves the level
	ladderFit      = 0.8              // a level fits from this share of its bitrate on
	ladderClimb    = 10 * time.Second // how long a heavier level must fit before the stream climbs to it
	ladderMaxClimb = 160 * time.Second
	ladderBounce   = 30 * time.Second // leaving a level this soon after climbing to it was a bad climb
	ladderSettled  = 2 * time.Minute  // a level kept this long resets the climb wait
	ladderMinGap   = 2 * time.Second  // between two changes
	ladderBottom   = 720              // the ladder ends at 720p: under it, text is hard to read
)

// presetRates are the frame rates of the call's presets, fastest first.
var presetRates = []int{144, 120, 60, 30}

// ladder decides the level a stream goes out at.
type ladder struct {
	all     []Level // the chosen preset first, then the lighter ones, heaviest first
	fps     int     // the frame rate the screens show best, from pickFPS
	at      Level   // the level sent now
	changed time.Time
	climbed time.Time     // when the stream last climbed
	since   time.Time     // since when a heavier level fits; zero while none does
	wait    time.Duration // how long a heavier level must fit
}

// newLadder takes the chosen preset and the presets under it (any order; heavier or larger ones
// are left out) and starts at the chosen one.
func newLadder(top Level, presets []Level) *ladder {
	l := &ladder{all: []Level{top}, fps: top.FPS, at: top, wait: ladderClimb}
	bottom := min(ladderBottom, top.Height)
	for _, p := range presets {
		if p != top && p.Width <= top.Width && p.Height <= top.Height && p.FPS <= top.FPS && p.Height >= bottom && p.Kbps < top.Kbps {
			l.all = append(l.all, p)
		}
	}
	// heaviest first; equal bitrates keep the faster frame rate first
	for i := 2; i < len(l.all); i++ {
		for j := i; j > 1 && (l.all[j].Kbps > l.all[j-1].Kbps || l.all[j].Kbps == l.all[j-1].Kbps && l.all[j].FPS > l.all[j-1].FPS); j-- {
			l.all[j], l.all[j-1] = l.all[j-1], l.all[j]
		}
	}
	return l
}

// usable lists the levels at the chosen frame rate, or at 60 or 30 under it, heaviest first.
func (l *ladder) usable() []Level {
	var out []Level
	for _, v := range l.all {
		if v.FPS == l.fps || v.FPS <= l.fps && (v.FPS == 60 || v.FPS == 30) {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = append(out, l.all[len(l.all)-1])
	}
	return out
}

// fitting returns the heaviest usable level the rate covers, or the lightest one.
func (l *ladder) fitting(kbps int) Level {
	use := l.usable()
	for _, v := range use {
		if float64(kbps) >= ladderFit*float64(v.Kbps) {
			return v
		}
	}
	return use[len(use)-1]
}

// start picks the first level for the rate the stream starts at.
func (l *ladder) start(kbps int, now time.Time) Level {
	l.at, l.changed = l.fitting(kbps), now
	return l.at
}

// next reports the level the stream should change to, and why ("bitrate" or "screens"); ok is
// false while it stays.
func (l *ladder) next(kbps int, now time.Time) (lv Level, why string, ok bool) {
	if now.Sub(l.changed) >= ladderSettled {
		l.wait = ladderClimb
	}
	best := l.fitting(kbps)
	usable := false
	for _, v := range l.usable() {
		usable = usable || v == l.at
	}
	switch {
	case now.Sub(l.changed) < ladderMinGap:
		return Level{}, "", false
	case !usable:
		why = "screens"
	case best.Kbps < l.at.Kbps && float64(kbps) < ladderLeave*float64(l.at.Kbps):
		if now.Sub(l.climbed) < ladderBounce {
			l.wait = min(2*l.wait, ladderMaxClimb)
		}
		why = "bitrate"
	case best.Kbps > l.at.Kbps:
		if l.since.IsZero() {
			l.since = now
		}
		if now.Sub(l.since) < l.wait {
			return Level{}, "", false
		}
		l.climbed = now
		why = "bitrate"
	default:
		l.since = time.Time{}
		return Level{}, "", false
	}
	l.at, l.changed, l.since = best, now, time.Time{}
	return best, why, true
}

// lowered says why the level sent now is not the chosen one: "screens" when the viewers' screens
// asked for another frame rate, "bitrate" when the rate does not cover it; "" when it is.
func (l *ladder) lowered() string {
	switch {
	case l.at == l.all[0]:
		return ""
	case l.fps != l.all[0].FPS && l.at == l.fitting(math.MaxInt32):
		return "screens"
	}
	return "bitrate"
}

// pickFPS chooses the frame rate a stream goes out at, among the presets' rates up to top: not
// above the source's refresh rate (it makes no more pictures) nor above the fastest viewer's
// screen (it shows no more), the one the most screens show evenly, ties going to the faster one.
// A screen shows a rate evenly when it shows each frame for the same number of refreshes, or
// every n-th frame of n per refresh. A rate of 0 is not known: such a viewer counts for nothing and
// does not cap the rate (it may be an older version, with any screen).
func pickFPS(top, sourceHz int, viewers []int) int {
	limit := top
	if sourceHz > 0 {
		limit = min(limit, sourceHz+1) // 59 Hz is 59.94: it shows 60 fps
	}
	fastest, unknown := 0, len(viewers) == 0
	for _, v := range viewers {
		fastest = max(fastest, v)
		unknown = unknown || v <= 0
	}
	if !unknown {
		limit = min(limit, fastest+1)
	}
	best, score := 0, -1
	for _, c := range append([]int{top}, presetRates...) {
		if c > limit || c > top || c == best {
			continue
		}
		n := 0
		if even(c, sourceHz) {
			n++
		}
		for _, v := range viewers {
			if even(c, v) {
				n++
			}
		}
		if n > score || n == score && c > best {
			best, score = c, n
		}
	}
	if best == 0 {
		return min(top, presetRates[len(presetRates)-1])
	}
	return best
}

// even reports whether a screen refreshing hz times a second shows fps frames a second in even
// steps; 59 Hz screens (59.94) show 60 fps evenly.
func even(fps, hz int) bool {
	if fps <= 0 || hz <= 0 {
		return false
	}
	r := float64(hz) / float64(fps)
	if r < 1 {
		r = 1 / r
	}
	k := math.Round(r)
	return math.Abs(r-k) <= 0.03*k
}
