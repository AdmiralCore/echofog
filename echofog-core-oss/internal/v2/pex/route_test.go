package pex

import (
	"testing"
	"time"
)

// TestPEXDropsNonRoutableCandidates reproduces ДР-2: a PEX answerer may sign a card with any
// address, and the node must not dial loopback, private, link-local (cloud metadata), CGNAT or
// ULA ranges. Host names cannot be judged before resolution and still pass.
func TestPEXDropsNonRoutableCandidates(t *testing.T) {
	hosts := []string{
		"127.0.0.1",       // loopback
		"10.0.0.5",        // private
		"192.168.1.10",    // private
		"169.254.169.254", // link-local / cloud metadata
		"100.64.0.1",      // CGNAT
		"fd00::1",         // ULA
	}
	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			st, err := Open(Options{KV: newMemKV(), Now: func() time.Time { return testNow }})
			if err != nil {
				t.Fatal(err)
			}
			c := testCard(t, cardOpts{name: "route-" + host, seq: 10, host: host})
			if st.AddCandidate(c, testNow) {
				t.Fatalf("AddCandidate accepted a candidate at %s", host)
			}
			if got := st.DialCandidates(10, testNow); len(got) != 0 {
				t.Fatalf("DialCandidates returned %d non-routable candidates", len(got))
			}
		})
	}

	// A public unicast address is still a candidate. Documentation ranges are reserved and not
	// routed (dialing them cannot reach a local service); they stay routable so the lab and the
	// node integration fixture can use them.
	for _, host := range []string{"8.8.8.8", "192.0.2.10"} {
		st, err := Open(Options{KV: newMemKV(), Now: func() time.Time { return testNow }})
		if err != nil {
			t.Fatal(err)
		}
		public := testCard(t, cardOpts{name: "route-public-" + host, seq: 10, host: host})
		if !st.AddCandidate(public, testNow) {
			t.Fatalf("AddCandidate refused %s", host)
		}
		if got := st.DialCandidates(10, testNow); len(got) != 1 {
			t.Fatalf("DialCandidates = %d cards for %s, want 1", len(got), host)
		}
	}

	// An explicit LAN/lab mode keeps the local classes too.
	lab, err := Open(Options{KV: newMemKV(), Now: func() time.Time { return testNow }, AllowNonRoutable: true})
	if err != nil {
		t.Fatal(err)
	}
	local := testCard(t, cardOpts{name: "route-lab", seq: 10, host: "127.0.0.1"})
	if !lab.AddCandidate(local, testNow) {
		t.Fatal("AllowNonRoutable did not keep a LAN address")
	}
}
