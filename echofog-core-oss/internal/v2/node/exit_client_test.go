package node_test

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// fakeExit is an exit that only records who connected.
type fakeExit struct{ peers chan node.Peer }

func registerFakeExit(t *testing.T, rt *node.Runtime) *fakeExit {
	t.Helper()
	spec, ok := service.Builtin(service.Exit)
	if !ok {
		t.Fatal("exit service is not built in")
	}
	fe := &fakeExit{peers: make(chan node.Peer, 4)}
	rt.Register(spec, func(_ context.Context, p node.Peer, st *mux.Stream) {
		fe.peers <- p
		_, _ = st.ReadFrame()
		_ = st.Close()
	})
	return fe
}

func (fe *fakeExit) seen(t *testing.T) node.Peer {
	t.Helper()
	select {
	case p := <-fe.peers:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("the exit saw no client")
	}
	return node.Peer{}
}

// services.md §7: a separate random account and device per exit; the exit never learns the
// messenger identity, and the identity of one exit is stable.
func TestExitClientUsesPerExitIdentity(t *testing.T) {
	client := newTestNode(t, cards.Hidden, false, nil, nil)
	qb, qc := newCarrierQueue(), newCarrierQueue()
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{qb}, nil)
	c := newTestNode(t, cards.Visible, true, []carrier.Listener{qc}, nil)
	exitB, exitC := registerFakeExit(t, b.rt), registerFakeExit(t, c.rt)
	b.start(t)
	c.start(t)
	client.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dial := func(q *carrierQueue, e *testNode, fe *fakeExit) node.Peer {
		pair := memcarrier.NewQUIC()
		q.push(pair.Server)
		cl, err := client.rt.ExitClient(ctx, e.peerCard(t), node.DialOpt{Carrier: pair.Client})
		if err != nil {
			t.Fatalf("ExitClient: %v", err)
		}
		defer cl.Close()
		conn, err := cl.DialExitStream(ctx, "192.0.2.1:80")
		if err != nil {
			t.Fatalf("DialExitStream: %v", err)
		}
		defer conn.Close()
		return fe.seen(t)
	}

	pb := dial(qb, b, exitB)
	pc := dial(qc, c, exitC)

	if pb.Node == client.id.NodeID || pb.Device == client.id.DeviceID {
		t.Fatal("exit B saw the messenger identity")
	}
	if pc.Node == client.id.NodeID || pc.Device == client.id.DeviceID {
		t.Fatal("exit C saw the messenger identity")
	}
	if pb.Node == pc.Node || pb.Device == pc.Device {
		t.Fatal("two exits saw the same client identity: they could link the client")
	}
	if pb.Card == nil || pb.Card.Body.GetFlags()&objects.FlagUnlisted == 0 {
		t.Fatal("the client card at the exit is not unlisted")
	}
	dl, err := objects.VerifyDeviceList(pb.Card.Body.GetDevices(), time.Now())
	if err != nil {
		t.Fatalf("client device list: %v", err)
	}
	if len(dl.Devices()) != 1 {
		t.Fatalf("client device list has %d devices, want 1", len(dl.Devices()))
	}

	n, d, err := client.rt.ExitIdentityIDs(ctx, b.id.DeviceID)
	if err != nil {
		t.Fatalf("ExitIdentityIDs: %v", err)
	}
	if n != pb.Node || d != pb.Device {
		t.Fatal("ExitIdentityIDs differs from what the exit saw")
	}
	n2, d2, err := client.rt.ExitIdentityIDs(ctx, b.id.DeviceID)
	if err != nil || n2 != n || d2 != d {
		t.Fatalf("the identity of an exit is not stable: %v", err)
	}
}
