package session_test

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/session"
)

// TestLiarInAnHonestPeersNetworkDoesNotBlockTheDecision: two honest peers in different /16
// networks and one later liar in the /16 of the first. The decision must still be made, and it
// must be the honest one.
func TestLiarInAnHonestPeersNetworkDoesNotBlockTheDecision(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.clk.advance(time.Second)
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.clk.advance(time.Second)
	r.o.Observe(obs(3, "198.51.100.50", "8.8.4.4:4000", "s")) // liar, same /16 as peer 1
	wantPublic(t, r.o, "8.8.8.8")
}

func TestLiarSeenBeforeQuorumDoesNotBlockEither(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.clk.advance(time.Second)
	r.o.Observe(obs(3, "198.51.100.50", "8.8.4.4:4000", "s")) // liar after the honest peer
	r.clk.advance(time.Second)
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "8.8.8.8")
}

func TestGroupMajorityBeatsOneLiar(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(3, "198.51.100.50", "8.8.4.4:4000", "s")) // liar seen first
	r.clk.advance(time.Second)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(4, "198.51.100.2", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "8.8.8.8")
}

// TestLiarPortInAnHonestNetworkDoesNotHideTheNAT: the same rule for NAT ports.
func TestLiarPortInAnHonestNetworkDoesNotHideTheNAT(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.clk.advance(time.Second)
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(3, "100.64.0.1", "8.8.8.8:4000", "s"))
	r.clk.advance(time.Second)
	r.o.Observe(obs(4, "198.51.100.50", "8.8.8.8:7777", "s")) // liar port in peer 1's /16
	if got := r.o.NAT("s"); got != session.NATCone {
		t.Fatalf("NAT = %v, want cone", got)
	}
}

// TestFloodFromOneNetworkDoesNotEvictHonestReports: Sybils of one /16 fill their own network's
// share and never push honest reports out of the table.
func TestFloodFromOneNetworkDoesNotEvictHonestReports(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(3, "100.64.0.1", "8.8.8.8:4000", "s"))
	for i := 0; i < 2*session.MaxObservations; i++ {
		r.clk.advance(time.Millisecond)
		r.o.Observe(session.Observation{
			Peer:     [32]byte{9, byte(i), byte(i >> 8)},
			PeerAddr: addrIn("45.9", i),
			Reported: ap("8.8.4.4:5000"),
			Socket:   "s",
		})
	}
	wantPublic(t, r.o, "8.8.8.8")
	if got := r.o.NAT("s"); got != session.NATCone {
		t.Fatalf("NAT = %v after the flood, want cone", got)
	}
}

// TestPerPeerReportsAreBounded: one peer cannot hold more than MaxSocketsPerPeer reports by
// naming many sockets.
func TestPerPeerReportsAreBounded(t *testing.T) {
	r := newObsRig(t)
	for i := 0; i < 50; i++ {
		r.clk.advance(time.Second)
		r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", fmt.Sprintf("sock%d", i)))
	}
	if n := r.o.Len(); n > session.MaxSocketsPerPeer {
		t.Fatalf("one peer holds %d reports, cap %d", n, session.MaxSocketsPerPeer)
	}
}

// addrIn returns the IPv4 address prefix.(i/250).(i%250+1), which stays inside one /16.
func addrIn(prefix string, i int) netip.Addr {
	return netip.MustParseAddr(fmt.Sprintf("%s.%d.%d", prefix, i/250, i%250+1))
}
