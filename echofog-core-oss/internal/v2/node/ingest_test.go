package node_test

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// A card a contact sent is verified and enters the card book; the book then answers for the device
// (T11.7: what the app layer's IngestCard does with a card that came as the answer to a knock).
func TestIngestPeerCardPutsAValidCardIntoTheBook(t *testing.T) {
	a := newTestNodeWith(t, cards.Visible, false, nil, nil, nil)
	b := newTestNodeWith(t, cards.Visible, false, nil, nil, nil)
	a.start(t)
	b.start(t)
	raw, ok := a.rt.Cards().Own(objects.ScopePeer)
	if !ok {
		t.Fatal("a has no peer card")
	}
	if b.rt.Cards().Peer(node.DeviceID(a.id.DeviceID)) != nil {
		t.Fatal("b knew the card before it was given")
	}
	if err := b.rt.IngestPeerCard(raw); err != nil {
		t.Fatalf("IngestPeerCard: %v", err)
	}
	if b.rt.Cards().Peer(node.DeviceID(a.id.DeviceID)) == nil {
		t.Fatal("the card is not in the book")
	}
}

// Garbage, a cut card and an empty one are refused and the book stays as it was.
func TestIngestPeerCardRefusesWhatDoesNotVerify(t *testing.T) {
	a := newTestNodeWith(t, cards.Visible, false, nil, nil, nil)
	b := newTestNodeWith(t, cards.Visible, false, nil, nil, nil)
	a.start(t)
	b.start(t)
	raw, _ := a.rt.Cards().Own(objects.ScopePeer)
	for name, in := range map[string][]byte{"nil": nil, "garbage": []byte("not a card"), "cut": raw[:len(raw)/2]} {
		if err := b.rt.IngestPeerCard(in); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if b.rt.Cards().Peer(node.DeviceID(a.id.DeviceID)) != nil {
		t.Fatal("a refused card reached the book")
	}
}
