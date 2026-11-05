package session_test

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/session"
)

// T3.5 acceptance F2: a table full of live entries must not lock honest keys out. Node IDs cost
// nothing and their entries live a day, so 4096 of them would otherwise refuse everybody.
func TestQuotaFullTableStillServesAnHonestKey(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	rule := session.Rule{Name: "deposit", PerSession: 1_000_000, Window: 10 * time.Second, PerNodeDay: 100}
	for i := 0; i < 5000; i++ {
		l.Allow(rule, [16]byte{1}, addr("10.0.0.1"), [32]byte{byte(i), byte(i >> 8), 2}) // one session, 5000 node IDs
	}
	clk.advance(11 * time.Second) // the short entries are dead, the node-day entries are not
	for i := 0; i < 5; i++ {
		if !l.Allow(rule, [16]byte{200, byte(i)}, addr("203.0.113.9"), [32]byte{200, byte(i)}) {
			t.Fatalf("honest key %d refused: a table of node-ID entries locked everybody out", i)
		}
	}
	if n := l.Tracked(); n > session.MaxTrackedSources {
		t.Fatalf("tracked = %d, want at most %d", n, session.MaxTrackedSources)
	}
}
