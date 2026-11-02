package network

import (
	"testing"
	"time"
)

// Keep-alives do not tick on a shared clock: QUIC moves each one, and every smux session draws
// its own interval. Both stay well under the 30 s idle timeout of v0.2.0 peers.
func TestKeepAlivesAreJittered(t *testing.T) {
	qc := GetQuicConfig()
	if qc.KeepAliveJitter < 0.2 || qc.KeepAliveJitter > 0.3 {
		t.Fatalf("QUIC keep-alive jitter %.2f, want 0.2..0.3", qc.KeepAliveJitter)
	}
	if latest := time.Duration(float64(qc.KeepAlivePeriod) * (1 + qc.KeepAliveJitter)); latest >= 25*time.Second {
		t.Fatalf("QUIC keep-alive may wait %v, too close to the peer's idle timeout", latest)
	}

	seen := map[time.Duration]bool{}
	for range 100 {
		d := smuxConfig().KeepAliveInterval
		if d < 7*time.Second || d > 13*time.Second {
			t.Fatalf("smux keep-alive %v outside 7..13 s", d)
		}
		seen[d.Round(100*time.Millisecond)] = true
	}
	if len(seen) < 10 {
		t.Fatalf("only %d distinct smux keep-alive intervals in 100 sessions", len(seen))
	}
}
