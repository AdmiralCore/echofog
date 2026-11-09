package node_test

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	apitypes "github.com/echofog/echofog-core/pkg/api/types"
)

// memDialer dials through the in-memory carrier into the listener queue of the other runtime, so
// links.Service.Redeem runs a real handshake between two runtimes of one process.
type memDialer struct {
	rt    *node.Runtime
	queue *carrierQueue
}

func (d memDialer) Session(ctx context.Context, peer node.DeviceID, opt node.DialOpt) (node.Peer, *mux.Session, error) {
	pair := memcarrier.NewQUIC()
	d.queue.push(pair.Server)
	opt.Carrier = pair.Client
	return d.rt.Session(ctx, peer, opt)
}

type liveApp struct {
	nd *testNode
	a  *app.App
	ln *links.Service
}

func newLiveApp(t *testing.T, queue *carrierQueue, peerQueue *carrierQueue) *liveApp {
	t.Helper()
	var ls []carrier.Listener
	if queue != nil {
		ls = []carrier.Listener{queue}
	}
	l := &liveApp{}
	l.nd = newTestNodeWith(t, cards.Visible, true, ls, nil, func(c *node.Config) {
		c.Chat = true
		c.ChatEvents.Control = func(from [32]byte, ctl *chatpb.Control) {
			if l.a != nil {
				l.a.OnControl(from, ctl)
			}
		}
	})
	l.nd.start(t)
	ln, err := links.New(links.Config{Cards: l.nd.builder, Dialer: memDialer{l.nd.rt, peerQueue}, Clock: l.nd.rt.Clock()})
	if err != nil {
		t.Fatal(err)
	}
	l.ln = ln
	a, err := app.New(app.Config{
		Runtime: l.nd.rt, Links: ln, Store: l.nd.rt.Store("t75b-app"), Clock: l.nd.rt.Clock(),
		Self: [32]byte(l.nd.id.NodeID), Chat: l.nd.rt.Chat(),
	})
	if err != nil {
		t.Fatal(err)
	}
	l.a = a
	return l
}

// Two real runtimes: B adds A by A's contact link (a real handshake through links.Service), the
// contact is saved, the session shows in Connected, and a knock is accepted by the real chat
// outbox.
func TestTwoRuntimesAddByLinkAndKnock(t *testing.T) {
	qA := newCarrierQueue()
	a := newLiveApp(t, qA, nil)
	b := newLiveApp(t, nil, qA)

	cl, err := a.ln.ContactLink(context.Background())
	if err != nil {
		t.Fatalf("ContactLink: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := b.a.AddByLink(ctx, cl.URL)
	if err != nil {
		t.Fatalf("AddByLink over a real session: %v", err)
	}
	wantNode := hex.EncodeToString(a.nd.id.NodeID[:])
	if c.Node != wantNode {
		t.Fatalf("contact %s, want %s", c.Node, wantNode)
	}
	if !b.a.Connected(apitypes.PeerID(a.nd.id.NodeID)) {
		t.Fatal("no live session with the contact after joining")
	}
	if got, _ := b.a.Contacts(); len(got) != 1 || got[0].Node != wantNode {
		t.Fatalf("contacts %+v", got)
	}
	if err := b.a.Knock(ctx, wantNode); err != nil {
		t.Fatalf("Knock through the real chat outbox: %v", err)
	}
}
