package pex

import (
	"testing"
	"time"
)

// TestPEXDropsNonRoutableCandidates reproduces ДР-2: a PEX answerer may sign a card with any
// address, and the node must not dial loopback, private, link-local (cloud metadata), CGNAT, ULA
// or documentation ranges. Host names cannot be judged before resolution and still pass.
func TestPEXDropsNonRoutableCandidates(t *testing.T) {
	hosts := []string{
		"127.0.0.1",       // loopback
		"10.0.0.5",        // private
		"192.168.1.10",    // private
		"169.254.169.254", // link-local / cloud metadata
		"100.64.0.1",      // CGNAT
		"fd00::1",         // ULA
		"192.0.2.10",      // documentation
		"198.51.100.1",    // documentation
		"203.0.113.1",     // documentation
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

	// A public unicast address is still a candidate.
	st, err := Open(Options{KV: newMemKV(), Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	public := testCard(t, cardOpts{name: "route-public", seq: 10, host: "8.8.8.8"})
	if !st.AddCandidate(public, testNow) {
		t.Fatal("AddCandidate refused a public address")
	}
	if got := st.DialCandidates(10, testNow); len(got) != 1 {
		t.Fatalf("DialCandidates = %d public cards, want 1", len(got))
	}

	// An explicit LAN/lab mode keeps them.
	lab, err := Open(Options{KV: newMemKV(), Now: func() time.Time { return testNow }, AllowNonRoutable: true})
	if err != nil {
		t.Fatal(err)
	}
	doc := testCard(t, cardOpts{name: "route-lab", seq: 10, host: "192.0.2.10"})
	if !lab.AddCandidate(doc, testNow) {
		t.Fatal("AllowNonRoutable did not keep a lab address")
	}
}
