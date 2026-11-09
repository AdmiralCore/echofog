package clock

import (
	"fmt"
	"testing"
	"time"
)

// TestStalePeerSamplesDoNotHoldTheOffset: handshake.md §7 (question 20) - a sample is valid
// for 24 hours; older samples are discarded and no longer hold the offset. Three to five
// liars that were fresh once must not keep the clock shifted after they expire.
func TestStalePeerSamplesDoNotHoldTheOffset(t *testing.T) {
	for _, liars := range []int{3, 4, 5} {
		t.Run(fmt.Sprintf("%d liars", liars), func(t *testing.T) {
			fc := newFakeClock(base)
			c := newClock(t, fc.Now, endpoints(3))
			for n := 1; n <= liars; n++ {
				c.AddOutgoingSample(netAddr(n), base.Add(100*time.Second))
			}
			if got := c.Offset(); got != 100*time.Second {
				t.Fatalf("fresh liars moved the offset to %v, want 100s", got)
			}
			fc.Advance(24*time.Hour + time.Second)
			// One honest outgoing session after the expiry: the liar samples are gone.
			c.AddOutgoingSample(netAddr(20), fc.Now())
			if got := c.Offset(); got != 0 {
				t.Fatalf("%d stale liar samples still hold the offset at %v", liars, got)
			}
		})
	}
}

// TestPeerSampleLivesFor24Hours: the expiry is inclusive, so a sample exactly 24 hours old
// still counts and keeps an agreeing median.
func TestPeerSampleLivesFor24Hours(t *testing.T) {
	fc := newFakeClock(base)
	c := newClock(t, fc.Now, endpoints(3))
	for n := 1; n <= 3; n++ {
		c.AddOutgoingSample(netAddr(n), base.Add(100*time.Second))
	}
	fc.Advance(24 * time.Hour)
	c.AddOutgoingSample(netAddr(4), fc.Now().Add(100*time.Second))
	if got := c.Offset(); got != 100*time.Second {
		t.Fatalf("Offset() = %v after 24h, want the samples still valid", got)
	}
}
