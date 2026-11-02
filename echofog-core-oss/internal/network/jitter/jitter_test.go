package jitter

import (
	"testing"
	"time"
)

func TestAroundStaysInRangeAndVaries(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 500 {
		d := Around(20*time.Second, Default)
		if d < 15*time.Second || d > 25*time.Second {
			t.Fatalf("%v is outside 15..25 s", d)
		}
		seen[d.Round(100*time.Millisecond)] = true
	}
	if len(seen) < 30 {
		t.Fatalf("only %d distinct waits in 500 draws", len(seen))
	}
	if d := Around(time.Second, 0); d != time.Second {
		t.Fatalf("no jitter asked, got %v", d)
	}
	if d := Around(time.Second, 3); d < 500*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("fraction not clamped: %v", d)
	}
}
