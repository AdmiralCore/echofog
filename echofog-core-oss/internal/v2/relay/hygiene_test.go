package relay

import (
	"errors"
	"testing"
	"time"
)

// L7 (accept 1): one IPv6 /64 is one source for the per-IP limit.
func TestPerIPCountsIPv6By64(t *testing.T) {
	w := newWorld(t, ServerConfig{Limits: Limits{PerIP: 1}})
	a1, a2 := w.join(newTNode(t), "[2001:db8:1:2::1]", true), w.join(newTNode(t), "[2001:db8:1:2::2]", true)
	other := w.join(newTNode(t), "[2001:db8:1:3::1]", true)
	b, c, d := w.join(newTNode(t), "10.0.0.2", true), w.join(newTNode(t), "10.0.0.3", true), w.join(newTNode(t), "10.0.0.4", true)
	if _, err := a1.dial(b); err != nil {
		t.Fatal(err)
	}
	_, err := a2.dial(c)
	var re *RefusedError
	if !errors.As(err, &re) || re.Reason != ReasonLimit {
		t.Fatalf("second host of the same /64: got %v, want REFUSED limit", err)
	}
	if _, err := other.dial(d); err != nil {
		t.Fatalf("another /64 must have its own budget: %v", err)
	}
}

// Rate buckets of peers that went quiet do not accumulate.
func TestRateBucketsArePruned(t *testing.T) {
	clk := newFakeClock(time.Now())
	s := NewServer(ServerConfig{Clock: clk})
	c := &circuit{mode: ModeStream, done: make(chan struct{})}
	for i := 0; i < 2000; i++ {
		var n [32]byte
		n[0], n[1] = byte(i), byte(i>>8)
		s.account(c, n, 1, false)
	}
	clk.Advance(5 * time.Second)
	var n [32]byte
	n[0] = 0xFF
	s.account(c, n, 1, false)
	s.mu.Lock()
	got := len(s.buckets)
	s.mu.Unlock()
	if got > 600 {
		t.Fatalf("%d buckets kept for peers idle for 5 s", got)
	}
}

// A circuit that never connected does not keep its ID for 24 hours.
func TestRefusedCircuitsDoNotKeepTheirIDs(t *testing.T) {
	w := newWorld(t, ServerConfig{})
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	b.pol = func(Incoming) bool { return false }
	for i := 0; i < 5; i++ {
		if _, err := a.dial(b); err == nil {
			t.Fatal("refusing target connected")
		}
	}
	waitFor(t, func() bool { return w.srv.ActiveCircuits() == 0 })
	w.srv.mu.Lock()
	used, order := len(w.srv.usedCIDs), len(w.srv.cidOrder)
	w.srv.mu.Unlock()
	if used != 0 || order != 0 {
		t.Fatalf("%d IDs (%d stamps) kept for circuits that never connected", used, order)
	}
}
