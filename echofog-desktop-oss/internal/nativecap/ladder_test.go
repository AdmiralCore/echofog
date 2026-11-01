package nativecap

import (
	"fmt"
	"testing"
	"time"
)

// the call's presets (tree.QualityPresets)
var testPresets = []Level{
	{640, 360, 30, 1000}, {1280, 720, 30, 3000}, {1280, 720, 60, 6000}, {1920, 1080, 30, 7000},
	{1920, 1080, 60, 12000}, {1920, 1080, 120, 20000}, {1920, 1080, 144, 24000}, {2560, 1440, 60, 20000},
	{2560, 1440, 120, 32000}, {2560, 1440, 144, 38000}, {3840, 2160, 30, 20000}, {3840, 2160, 60, 35000},
}

func lv(name string) Level {
	for _, p := range testPresets {
		if fmt.Sprintf("%dp%d", p.Height, p.FPS) == name {
			return p
		}
	}
	panic(name)
}

func names(ls []Level) string {
	s := ""
	for _, l := range ls {
		s += fmt.Sprintf(" %dp%d", l.Height, l.FPS)
	}
	return s
}

func TestLadderKeepsThePresetsUnderTheChosenOne(t *testing.T) {
	l := newLadder(lv("1440p144"), testPresets)
	if got, want := names(l.all), " 1440p144 1440p120 1080p144 1080p120 1440p60 1080p60 1080p30 720p60 720p30"; got != want {
		t.Fatalf("ladder%s, want%s", got, want)
	}
	// 120 fps on a 144 Hz picture goes in uneven steps: the ladder keeps to 144, then 60 and 30
	if got, want := names(l.usable()), " 1440p144 1080p144 1440p60 1080p60 1080p30 720p60 720p30"; got != want {
		t.Fatalf("usable%s, want%s", got, want)
	}
	if got := names(newLadder(lv("720p30"), testPresets).all); got != " 720p30" {
		t.Fatalf("a 720p30 stream has nowhere to go, got%s", got)
	}
}

// The link of the user's test carried about 12 Mbit/s: a 1080p144 stream goes to 1080p60, where
// those bits are worth twice as much per pixel, and goes back only after the rate covered 1080p144
// for a while.
func TestLadderGoesDownWhenTheRateFallsAndClimbsBackSlowly(t *testing.T) {
	start := time.Unix(0, 0)
	l := newLadder(lv("1080p144"), testPresets)
	if got := l.start(24000, start); got != lv("1080p144") {
		t.Fatalf("started at %dp%d", got.Height, got.FPS)
	}
	now := start.Add(5 * time.Second)
	if _, _, ok := l.next(16000, now); ok {
		t.Fatal("2/3 of the level's rate is not worth a new encoder")
	}
	got, why, ok := l.next(12000, now)
	if !ok || got != lv("1080p60") || why != "bitrate" {
		t.Fatalf("at 12 Mbit/s: %dp%d %q %v, want 1080p60 for the bitrate", got.Height, got.FPS, why, ok)
	}
	if l.lowered() != "bitrate" {
		t.Fatalf("lowered %q", l.lowered())
	}
	if _, _, ok := l.next(3000, now.Add(time.Second)); ok {
		t.Fatal("two changes within a second")
	}
	// the rate comes back: 1080p144 fits from 19.2 Mbit/s, for ten seconds
	now = now.Add(20 * time.Second)
	for i := 0; i < 9; i++ {
		if _, _, ok := l.next(20000, now.Add(time.Duration(i)*time.Second)); ok {
			t.Fatalf("climbed after %d s", i)
		}
	}
	got, _, ok = l.next(20000, now.Add(10*time.Second))
	if !ok || got != lv("1080p144") || l.lowered() != "" {
		t.Fatalf("after 10 s: %dp%d %v, want 1080p144", got.Height, got.FPS, ok)
	}
	// it was too soon: the path backs up again, and the next climb waits twice as long
	now = now.Add(15 * time.Second)
	if got, _, ok = l.next(11000, now); !ok || got != lv("1080p60") {
		t.Fatalf("at 11 Mbit/s: %dp%d %v", got.Height, got.FPS, ok)
	}
	now = now.Add(5 * time.Second)
	for i := 0; i <= 19; i++ {
		if _, _, ok := l.next(24000, now.Add(time.Duration(i)*time.Second)); ok {
			t.Fatalf("climbed after %d s of the doubled wait", i)
		}
	}
	if _, _, ok := l.next(24000, now.Add(20*time.Second)); !ok {
		t.Fatal("did not climb after 20 s")
	}
}

func TestLadderFollowsTheScreensFrameRate(t *testing.T) {
	start := time.Unix(0, 0)
	l := newLadder(lv("1440p144"), testPresets)
	l.start(38000, start)
	l.fps = 60 // a viewer with a 60 Hz screen joined
	got, why, ok := l.next(38000, start.Add(3*time.Second))
	if !ok || got != lv("1440p60") || why != "screens" || l.lowered() != "screens" {
		t.Fatalf("got %dp%d %q %v (%q), want 1440p60 for the screens", got.Height, got.FPS, why, ok, l.lowered())
	}
	// the 60 Hz viewer left, the others have 120 Hz screens: a climb, which waits like any other
	l.fps = 120
	if _, _, ok := l.next(38000, start.Add(6*time.Second)); ok {
		t.Fatal("climbed at once")
	}
	got, _, ok = l.next(38000, start.Add(16*time.Second))
	if !ok || got != lv("1440p120") {
		t.Fatalf("at 120: %dp%d %v", got.Height, got.FPS, ok)
	}
}

func TestPickFPS(t *testing.T) {
	cases := []struct {
		top, source int
		viewers     []int
		want        int
	}{
		{144, 144, nil, 144},
		{144, 144, []int{144}, 144},
		{144, 144, []int{60}, 60},       // a 60 Hz screen shows no more than 60
		{144, 144, []int{144, 60}, 144}, // the source and one viewer show 144 evenly
		{144, 144, []int{0}, 144},       // an unknown screen does not cap it
		{144, 144, []int{0, 60}, 144},   // ... even beside a known one
		{144, 60, nil, 60},              // a 60 Hz monitor makes no more pictures
		{144, 120, []int{60, 144}, 120}, // 120 on the source and the 60 Hz screen, over 60 on both
		{144, 120, []int{120, 120, 60}, 120},
		{60, 59, []int{59}, 60},          // 59.94 Hz
		{144, 165, []int{165}, 144},      // nothing is even: the fastest
		{120, 144, []int{144, 144}, 120}, // never above the preset
		{144, 75, []int{75}, 60},         // capped by the source
		{30, 144, []int{60}, 30},
	}
	for _, c := range cases {
		if got := pickFPS(c.top, c.source, c.viewers); got != c.want {
			t.Errorf("pickFPS(%d, %d, %v) = %d, want %d", c.top, c.source, c.viewers, got, c.want)
		}
	}
}
