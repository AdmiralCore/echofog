package node_test

import (
	"context"
	"crypto/rand"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/punch"
)

// A card handed to Session directly is not in the card book, yet hole punching needs it: the lookup
// is lazy and falls back to the live session (T6.7, d62fea51). The same punch network built before
// the session must resolve the card once the session is in the table.
func TestPunchFindsCardOfLiveSessionLazily(t *testing.T) {
	var sk [32]byte
	_, _ = rand.Read(sk[:])
	p := &node.Noise3Punch{}
	a := newTestNodeFull(t, cards.Visible, false, nil, nil, nil, func(c *node.Config) {
		c.PunchNet = p
		c.ListenerSources = []node.ListenerSource{node.Noise3Listener{
			Addr:  "127.0.0.1:0",
			Masks: []noise3.MaskKey{{Private: sk, NotAfter: time.Now().Add(time.Hour)}},
			Punch: p,
		}}
	})
	bq := newCarrierQueue()
	b := newTestNodeFull(t, cards.Visible, false, []carrier.Listener{bq}, nil, nil, nil)
	a.start(t)
	b.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if card := node.PeerCardOf(a.rt, b.id.DeviceID); card != nil {
		t.Fatalf("the card book already holds %x", b.id.DeviceID)
	}
	// The engine builds its network before the session exists: no card yet.
	n := p.Net(b.id.DeviceID, punch.Initiator)
	target := []netip.AddrPort{netip.MustParseAddrPort("203.0.113.9:5000")}
	before, stopBefore := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err := n.Dial(before, target, 24)
	stopBefore()
	if err == nil || !strings.Contains(err.Error(), "without targets or card") {
		t.Fatalf("before the session: %v, want the missing-card error", err)
	}

	// The card is given to Session directly, so only the session can supply it.
	pair := memcarrier.NewQUIC()
	bq.push(pair.Server)
	if _, _, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{Card: b.peerCard(t), Carrier: pair.Client}); err != nil {
		t.Fatalf("session: %v", err)
	}
	card := node.PeerCardOf(a.rt, b.id.DeviceID)
	if card == nil || card.DeviceID != b.id.DeviceID {
		t.Fatalf("the card of the live session is not found lazily: %+v", card)
	}

	// The same lazy network now has the card: the missing-card error is gone.
	after, stopAfter := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = n.Dial(after, target, 24)
	stopAfter()
	if err != nil && strings.Contains(err.Error(), "without targets or card") {
		t.Fatalf("after the session the card is still missing: %v", err)
	}
}
