package exit

import (
	"testing"
)

func TestCoreResultCodes(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		in   byte
		want uint32
	}{{0, ResultOK}, {1, ResultDenied}, {2, ResultUnreachable}, {3, ResultPaymentRequired}, {4, ResultBusy}, {9, ResultUnreachable}} {
		if got := coreResultCode(c.in); got != c.want {
			t.Fatalf("coreResultCode(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestUDPSessionLimit(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(ServerConfig{Limits: Limits{UDPSessions: 2}})
	if err != nil {
		t.Fatal(err)
	}
	node := [32]byte{1}
	if !srv.acquireUDP(node) || !srv.acquireUDP(node) {
		t.Fatal("two sessions refused")
	}
	if srv.acquireUDP(node) {
		t.Fatal("third session allowed")
	}
	if !srv.acquireUDP([32]byte{2}) {
		t.Fatal("the limit leaked to another identity")
	}
	srv.releaseUDP(node)
	if !srv.acquireUDP(node) {
		t.Fatal("session refused after release")
	}
}

func TestUDPTargetLimit(t *testing.T) {
	t.Parallel()

	a := &udpAdapter{maxTargets: 2, targets: map[string]struct{}{}}
	if !a.admitTarget("a:1") || !a.admitTarget("b:2") {
		t.Fatal("first two targets refused")
	}
	if a.admitTarget("c:3") {
		t.Fatal("257th-style target allowed over the limit")
	}
	if !a.admitTarget("a:1") {
		t.Fatal("a repeated target was refused")
	}
}

func TestHandleDatagramDropsUnknownAndMalformed(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	peer := Peer{Node: [32]byte{7}}
	srv.HandleDatagram(peer, nil)
	srv.HandleDatagram(peer, []byte{0xff})
	if got := srv.Stats().DatagramsDropped.Load(); got != 2 {
		t.Fatalf("dropped = %d, want 2", got)
	}
	// A well-formed fragment for a session that does not exist is also dropped.
	f := Fragment{Session: 5, Packet: 1, Index: 0, Count: 1, Addr: "a:1", Data: []byte("x")}
	srv.HandleDatagram(peer, f.Append(nil))
	if got := srv.Stats().DatagramsDropped.Load(); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
	if got := srv.Stats().FragmentsReassembled.Load(); got != 0 {
		t.Fatalf("reassembled = %d, want 0", got)
	}
}

func TestPacketLimitDropsBurst(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(ServerConfig{Limits: Limits{UDPPacketsPerSec: 1}})
	if err != nil {
		t.Fatal(err)
	}
	peer := Peer{Node: [32]byte{9}}
	f := Fragment{Session: 5, Packet: 1, Index: 0, Count: 1, Addr: "a:1", Data: []byte("x")}
	srv.HandleDatagram(peer, f.Append(nil))
	srv.HandleDatagram(peer, f.Append(nil))
	if got := srv.Stats().DatagramsDropped.Load(); got != 2 {
		// Both are dropped because the session does not exist; the second must not even reach
		// the lookup. The counter alone cannot tell them apart, so check the rate limiter.
		t.Fatalf("dropped = %d, want 2", got)
	}
	if srv.packet.allow(srv.clk.Now(), peer.Node) {
		t.Fatal("rate limiter still has a token")
	}
}
