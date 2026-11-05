package node_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

func observation(n byte, peerAddr, reported string) session.Observation {
	return session.Observation{
		Peer:     [32]byte{n},
		PeerAddr: netip.MustParseAddr(peerAddr),
		Reported: netip.MustParseAddrPort(reported),
		Socket:   "s",
	}
}

// TestPublicAddrDecisionReissuesCardsAndEmitsEvent: two peers in different /16 networks agree,
// the runtime event fires, and the card of a following listener carries the new host with a
// higher seq. A single liar changes nothing.
func TestPublicAddrDecisionReissuesCardsAndEmitsEvent(t *testing.T) {
	ev := &captureEvents{}
	n := newTestNodeFull(t, cards.Visible, true, nil, ev, func(c *cards.Config) {
		c.Listeners[0].FollowPublic = true
	}, nil)
	obs := node.ObserverOf(n.rt)

	before, err := n.builder.Current(t.Context(), objects.ScopePeer)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if _, ok := n.rt.PublicAddr(); ok {
		t.Fatal("public address known before any report")
	}

	obs.Observe(observation(1, "198.51.100.1", "203.0.113.5:4000"))
	obs.Observe(observation(9, "100.64.0.1", "203.0.113.99:4000")) // a liar
	if ev.has(node.EventPublicAddr) {
		t.Fatal("event before the agreement")
	}
	obs.Observe(observation(2, "192.0.2.9", "203.0.113.5:4000"))

	got, ok := n.rt.PublicAddr()
	if !ok || got != netip.MustParseAddr("203.0.113.5") {
		t.Fatalf("PublicAddr = %v %v", got, ok)
	}
	e, ok := ev.last(node.EventPublicAddr)
	if !ok || e.PublicAddr != got {
		t.Fatalf("event = %+v %v", e, ok)
	}
	after, err := n.builder.Current(t.Context(), objects.ScopePeer)
	if err != nil {
		t.Fatalf("Current after: %v", err)
	}
	if after.Seq <= before.Seq {
		t.Fatalf("card seq %d did not grow past %d", after.Seq, before.Seq)
	}
	card, err := objects.VerifyCard(after.Envelope, time.Now())
	if err != nil {
		t.Fatalf("VerifyCard: %v", err)
	}
	if host := card.Body.GetAddrs()[0].GetHost(); host != "203.0.113.5" {
		t.Fatalf("card host = %q", host)
	}
	if c := n.rt.NATClass(); c != session.NATUnknown {
		t.Fatalf("NATClass = %v with 2 reports", c)
	}
}
