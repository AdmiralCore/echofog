package node_test

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
)

// TestPublicAddrEventUsesTheInjectedClock (AD13): the event time is the node's corrected clock,
// not the wall clock.
func TestPublicAddrEventUsesTheInjectedClock(t *testing.T) {
	fixed := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	clk, err := clock.New(clock.Config{Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatalf("clock.New: %v", err)
	}
	ev := &captureEvents{}
	n := newTestNodeFull(t, cards.Visible, true, nil, ev, func(c *cards.Config) {
		c.Listeners[0].FollowPublic = true
	}, func(c *node.Config) {
		c.Clock = clk
		c.CarrierClock = fakeCarrierClock{raw: &fakeRaw{now: fixed}}
	})
	obs := node.ObserverOf(n.rt)
	obs.Observe(observation(1, "198.51.100.1", "8.8.8.8:4000"))
	obs.Observe(observation(2, "192.0.2.9", "8.8.8.8:4000"))
	e, ok := ev.last(node.EventPublicAddr)
	if !ok {
		t.Fatal("no EventPublicAddr")
	}
	if !e.At.Equal(fixed) {
		t.Fatalf("event time %v, want the injected clock %v", e.At, fixed)
	}
}
