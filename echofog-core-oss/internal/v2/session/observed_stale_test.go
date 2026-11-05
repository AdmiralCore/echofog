package session_test

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/session"
)

// TestDecisionExpiresWithItsEvidence: when every report has aged out the node does not know its
// address any more (the uplink may have changed while nothing was observed).
func TestDecisionExpiresWithItsEvidence(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "8.8.8.8")

	r.clk.advance(48 * time.Hour)
	wantPublic(t, r.o, "")
	if r.o.Len() != 0 {
		t.Fatalf("Len = %d", r.o.Len())
	}
	// a single new report does not bring the old decision back, nor make a new one
	r.o.Observe(obs(3, "100.64.0.1", "8.8.4.4:4000", "s"))
	wantPublic(t, r.o, "")
	if got := r.o.NATClass(); got != session.NATUnknown {
		t.Fatalf("NATClass = %v", got)
	}
}

// TestFreshDecisionAfterStalenessIsNotRateLimited: an address learned after the old decision
// expired is a first decision, not a change.
func TestFreshDecisionAfterStalenessIsNotRateLimited(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.clk.advance(session.ObservationTTL + time.Minute)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.4.4:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.4.4:4000", "s"))
	wantPublic(t, r.o, "8.8.4.4")
}
