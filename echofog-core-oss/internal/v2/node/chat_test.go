package node_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

func chatNode(t *testing.T, vis cards.Visibility, listeners []carrier.Listener) *testNode {
	t.Helper()
	return newTestNodeWith(t, vis, true, listeners, nil, func(c *node.Config) { c.Chat = true })
}

// ownPrekeys is the prekeys/2 among the node's own DHT records.
func ownPrekeys(t *testing.T, nd *testNode) (*objects.Prekeys, []byte) {
	t.Helper()
	for _, rec := range node.OwnDHTRecords(nd.rt) {
		if p, err := objects.VerifyPrekeys(rec, time.Now()); err == nil {
			return p, rec
		}
	}
	return nil, nil
}

// A node with chat/2 publishes its prekeys/2 next to its device list and card (objects.md 5).
func TestChatPublishesPrekeysAndTheCardCarriesTheirHash(t *testing.T) {
	nd := chatNode(t, cards.Visible, nil)
	nd.start(t)

	p, raw := ownPrekeys(t, nd)
	if p == nil {
		t.Fatal("no prekeys/2 among the own DHT records")
	}
	if p.DeviceID != [32]byte(nd.id.DeviceID) {
		t.Fatal("prekeys/2 is not signed by the device key")
	}
	if len(p.Body.GetOpks()) != 0 {
		t.Fatal("the published copy carries one-time prekeys")
	}
	want := sha256.Sum256(raw)
	card := nd.peerCard(t)
	if !bytes.Equal(card.Body.GetPrekeysHash(), want[:]) {
		t.Fatalf("card prekeys_hash %x, want %x", card.Body.GetPrekeysHash(), want[:])
	}
}

func TestChatDoesNotPublishForAnUnlistedNode(t *testing.T) {
	nd := chatNode(t, cards.Hidden, nil)
	nd.start(t)
	if p, _ := ownPrekeys(t, nd); p != nil {
		t.Fatal("a hidden node published its prekeys")
	}
}

func TestNodeWithoutChatFlagOffersNoChat(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue}, nil, nil)
	b.start(t)
	a.start(t)
	if st, err := openChat(t, a, b, queue); err == nil {
		_ = st.Close()
		t.Fatal("chat/2 was offered without Config.Chat")
	}
}

func openChat(t *testing.T, a, b *testNode, queue *carrierQueue) (*mux.Stream, error) {
	t.Helper()
	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, st, err := a.rt.Open(ctx, b.id.DeviceID, service.Chat, node.DialOpt{Card: b.publicCard(t), Carrier: pair.Client})
	return st, err
}

// BUNDLE_GET over a real direct session answers with a prekeys/2 that has one one-time prekey.
func TestChatServesBundleGetOverASession(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := chatNode(t, cards.Visible, []carrier.Listener{queue})
	b.start(t)
	a.start(t)

	st, err := openChat(t, a, b, queue)
	if err != nil {
		t.Fatalf("Open chat: %v", err)
	}
	defer st.Close()
	body, _ := objects.Marshal(&chatpb.BundleGet{Req: 5})
	typ, payload := ask(t, st, 0x42, body)
	if typ != 0x43 {
		t.Fatalf("frame type 0x%x, want BUNDLE", typ)
	}
	m, err := chatpb.ParseBundle(payload)
	if err != nil || m.Req != 5 {
		t.Fatalf("BUNDLE: %v %v", m, err)
	}
	p, err := objects.VerifyPrekeys(m.Prekeys, time.Now())
	if err != nil {
		t.Fatalf("the bundle does not verify: %v", err)
	}
	if p.DeviceID != [32]byte(b.id.DeviceID) || len(p.Body.GetOpks()) != 1 {
		t.Fatalf("device %x, %d one-time prekeys", p.DeviceID, len(p.Body.GetOpks()))
	}
}
