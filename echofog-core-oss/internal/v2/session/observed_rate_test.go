package session_test

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/session"
)

// TestPublicAddrChangesAreRateLimited: a flapping majority (or a Sybil crowd that keeps
// outvoting the honest peers) must not reissue the card more often than the minimum interval.
func TestPublicAddrChangesAreRateLimited(t *testing.T) {
	r := newObsRig(t)
	a, b := "8.8.8.8:4000", "8.8.4.4:4000"
	r.o.Observe(obs(1, "198.51.100.1", a, "s"))
	r.o.Observe(obs(2, "192.0.2.9", a, "s"))
	wantPublic(t, r.o, "8.8.8.8")

	// flip immediately: refused, the decision stays
	r.o.Observe(obs(1, "198.51.100.1", b, "s"))
	r.o.Observe(obs(2, "192.0.2.9", b, "s"))
	wantPublic(t, r.o, "8.8.8.8")
	if r.nChanges() != 1 {
		t.Fatalf("changes = %d, want 1", r.nChanges())
	}

	// once the interval has passed the standing majority is applied by the next report
	r.clk.advance(session.MinChangeInterval + time.Second)
	r.o.Observe(obs(2, "192.0.2.9", b, "s"))
	wantPublic(t, r.o, "8.8.4.4")
	if r.nChanges() != 2 {
		t.Fatalf("changes = %d, want 2", r.nChanges())
	}
}

func TestFlappingNeverChangesFasterThanInterval(t *testing.T) {
	r := newObsRig(t)
	addrs := []string{"8.8.8.8:4000", "8.8.4.4:4000"}
	total := 60 * time.Minute
	for i := 0; time.Duration(i)*time.Minute < total; i++ {
		ip := addrs[i%2]
		r.o.Observe(obs(1, "198.51.100.1", ip, "s"))
		r.o.Observe(obs(2, "192.0.2.9", ip, "s"))
		r.clk.advance(time.Minute)
	}
	max := 1 + int(total/session.MinChangeInterval)
	if got := r.nChanges(); got > max {
		t.Fatalf("%d changes in %v, want at most %d", got, total, max)
	}
}

func TestResetAllowsAnImmediateNewDecision(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.o.Reset() // the uplink changed
	r.o.Observe(obs(1, "198.51.100.1", "8.8.4.4:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.4.4:4000", "s"))
	wantPublic(t, r.o, "8.8.4.4")
}
