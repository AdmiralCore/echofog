package node_test

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// TestDatagramPortSendReusesLiveSessionWithoutCard: a module that accepted a session (or opened
// one without storing the card) must be able to answer a datagram on it. The runtime already has
// the verified session; requiring a card from the card book would make the echo service unable to
// reply to the peer that just sent it a datagram (T0.3).
func TestDatagramPortSendReusesLiveSessionWithoutCard(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	mod := &fakeModule{got: make(chan node.Peer, 1), errs: make(chan error, 1)}
	registerFakeModule(t, a.rt, mod)
	registerFakeModule(t, b.rt, mod)
	b.start(t)
	a.start(t)

	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, stream, err := a.rt.Open(ctx, b.id.DeviceID, service.Chat, node.DialOpt{Card: b.peerCard(t), Carrier: pair.Client})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()

	portA := a.rt.Datagrams(frame.DgramCallAudio)
	defer portA.Close()
	portB := b.rt.Datagrams(frame.DgramCallAudio)
	defer portB.Close()

	if err := portA.Send(ctx, b.id.DeviceID, frame.DgramCallAudio, []byte("media")); err != nil {
		t.Fatalf("datagram send without a stored card: %v", err)
	}
	d, err := portB.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if string(d.Payload) != "media" || d.Peer.Device != a.id.DeviceID {
		t.Fatalf("datagram %+v from %x, want media from %x", d, d.Peer.Device, a.id.DeviceID)
	}
}
