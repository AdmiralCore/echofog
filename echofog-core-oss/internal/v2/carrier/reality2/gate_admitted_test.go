package reality2

import (
	"fmt"
	"testing"
	"time"
)

// TestGateAdmittedTableIsBounded reproduces НС-1: a stranger with a public card can admit an
// unbounded number of 4-tuples, and the admitted table never shrinks. The table must be an LRU
// with a hard cap, and the evictions must be counted.
func TestGateAdmittedTableIsBounded(t *testing.T) {
	conn := newFakeConn()
	cover := &fakeCover{}
	clk := newTestClock(time.Unix(1793664000, 0))
	g, err := NewGate(conn, GateConfig{
		Masks:       []MaskKey{{Private: mustMask(t)}},
		Addrs:       []string{testAddr},
		Cover:       cover,
		Replay:      NewReplay(ReplayConfig{}),
		MaxAdmitted: 8,
		Now:         clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })

	for i := 0; i < 100; i++ {
		g.admit(fakeAddr(fmt.Sprintf("192.0.2.%d:1000", i)).String())
	}
	if n := len(g.admitted); n > 8 {
		t.Fatalf("admitted table holds %d entries, want at most 8", n)
	}
	if g.AdmittedEvicted() == 0 {
		t.Fatal("evictions of admitted 4-tuples are not counted")
	}
	if !g.isAdmitted("192.0.2.99:1000") {
		t.Fatal("the newest admitted source was evicted")
	}
	if g.isAdmitted("192.0.2.0:1000") {
		t.Fatal("the oldest admitted source survived LRU eviction")
	}
}

// TestGateAdmittedExpiresByTTL reproduces the second half of НС-1: a source that went silent
// longer than admittedIdle must be dropped by the background sweep, not only when it speaks again.
func TestGateAdmittedExpiresByTTL(t *testing.T) {
	conn := newFakeConn()
	cover := &fakeCover{}
	clk := newTestClock(time.Unix(1793664000, 0))
	g, err := NewGate(conn, GateConfig{
		Masks:  []MaskKey{{Private: mustMask(t)}},
		Addrs:  []string{testAddr},
		Cover:  cover,
		Replay: NewReplay(ReplayConfig{}),
		Now:    clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })

	key := "198.51.100.7:2000"
	g.admit(key)
	if !g.isAdmitted(key) {
		t.Fatal("a fresh 4-tuple is not admitted")
	}
	clk.Advance(admittedIdle + time.Second)
	g.reapExpired()
	if n := len(g.admitted); n != 0 {
		t.Fatalf("admitted table holds %d entries after the idle TTL, want 0", n)
	}
	if g.isAdmitted(key) {
		t.Fatal("a silent 4-tuple survived the idle TTL")
	}
}
