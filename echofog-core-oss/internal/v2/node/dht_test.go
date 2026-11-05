package node_test

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
)

// TestRoutingTableLearnsSessionPeers pins the T5.1a module registration: every authenticated
// session adds the peer's signed card to the DHT routing table (dht.md §5), and the table exists
// as soon as the runtime is started.
func TestRoutingTableLearnsSessionPeers(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Visible, true, nil, nil)
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)

	b.start(t)
	a.start(t)

	if a.rt.RoutingTable() == nil || b.rt.RoutingTable() == nil {
		t.Fatal("the runtime did not open its routing table")
	}

	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: pair.Client,
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_ = sess.Close()

	wait := func(name string, rt *dht.Table, want node.DeviceID) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if rt.Has(dht.DeviceID(want)) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("%s did not add the peer to its routing table", name)
	}
	wait("initiator", a.rt.RoutingTable(), b.id.DeviceID)
	wait("responder", b.rt.RoutingTable(), a.id.DeviceID)
}
