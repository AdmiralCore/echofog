package node_test

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
)

// T3.5 acceptance F4 (AD13): the limits of the node run on the injected carrier clock, so the
// daily rotation of the puzzle secret R, the ban windows and the rate windows can be moved by a
// test and never read the wall clock behind the runtime's back.
func TestNodeLimitsFollowTheInjectedClock(t *testing.T) {
	raw := &fakeRaw{now: time.Now()}
	n := newTestNodeWith(t, cards.Visible, true, nil, nil, func(c *node.Config) {
		c.CarrierClock = fakeCarrierClock{raw: raw}
	})
	lim := node.LimitsOf(n.rt)
	if lim.Puzzle().PrevSecret != nil {
		t.Fatal("a fresh node already has a previous puzzle secret")
	}
	raw.mu.Lock()
	raw.now = raw.now.Add(25 * time.Hour)
	raw.mu.Unlock()
	if lim.Puzzle().PrevSecret == nil {
		t.Fatal("R did not rotate after 25 h of injected time: Limits reads the wall clock")
	}
}
