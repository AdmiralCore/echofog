package nativecap

import (
	"testing"
	"time"
)

func TestOutputSize(t *testing.T) {
	cases := []struct{ w, h, maxW, maxH, ow, oh int }{
		{2560, 1440, 2560, 1440, 2560, 1440},
		{2560, 1440, 1920, 1080, 1920, 1080},
		{1920, 1026, 2560, 1440, 1920, 1026}, // never enlarged
		{1921, 1027, 2560, 1440, 1920, 1026}, // even
		{3440, 1440, 2560, 1440, 2560, 1070}, // ultrawide: width bound
		{1080, 1920, 1920, 1080, 606, 1080},  // portrait: height bound
		{0, 0, 1280, 720, 1280, 720},
	}
	for _, c := range cases {
		if w, h := outputSize(c.w, c.h, c.maxW, c.maxH); w != c.ow || h != c.oh {
			t.Errorf("outputSize(%d, %d, %d, %d) = %dx%d, want %dx%d", c.w, c.h, c.maxW, c.maxH, w, h, c.ow, c.oh)
		}
	}
}

// A 144 Hz capture into a 120 fps beat: every beat sends, either on time or at the next picture.
func TestFrameClockKeepsTheTargetRateFromAFasterUnevenCapture(t *testing.T) {
	start := time.Unix(0, 0)
	c := newFrameClock(120, start)
	sent := 0
	// pictures every 1/144 s, but every 7th one is held back almost a refresh, as a busy
	// compositor does: the rate stays 144, the gaps do not
	period := time.Second / 144
	arrival := func(k int) time.Time {
		at := start.Add(time.Duration(k) * period)
		if k%7 == 0 {
			at = at.Add(period * 9 / 10)
		}
		return at
	}
	k := 1
	for now := start; now.Before(start.Add(10 * time.Second)); now = now.Add(100 * time.Microsecond) {
		for !now.Before(arrival(k)) {
			k++
			if c.captured() {
				c.sent(now)
				sent++
			}
		}
		if !now.Before(c.next) && c.beat(now) {
			c.sent(now)
			sent++
		}
	}
	if sent < 1150 || sent > 1201 {
		t.Fatalf("sent %d frames in 10 s, want about 1200", sent)
	}
}

func TestFrameClockRepeatsAStillPicture(t *testing.T) {
	start := time.Unix(0, 0)
	c := newFrameClock(60, start)
	sent := 0
	for now := start; now.Before(start.Add(2 * time.Second)); now = now.Add(time.Millisecond) {
		if !now.Before(c.next) && c.beat(now) {
			c.sent(now)
			sent++
		}
	}
	// one repeat per minFrameGap, not one per beat
	if want := int(2 * time.Second / minFrameGap); sent < want-2 || sent > want+1 {
		t.Fatalf("sent %d frames of a still picture in 2 s, want about %d", sent, want)
	}
}
