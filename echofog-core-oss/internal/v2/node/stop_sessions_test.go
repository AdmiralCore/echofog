package node_test

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// F1 of the T11.7 acceptance: Stop ends the sessions of the runtime, it does not leave them draining for
// the 15 s of a GOAWAY (wire.md §7.3) with their loops running: the peer sees the session end at once.
func TestStopEndsTheSessionsOfTheRuntime(t *testing.T) {
	for _, stopper := range []string{"dialer", "listener"} {
		t.Run(stopper, func(t *testing.T) {
			queue := newCarrierQueue()
			a := newTestNode(t, cards.Hidden, false, nil, nil)
			b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
			b.start(t)
			a.start(t)
			pair := memcarrier.NewQUIC()
			queue.push(pair.Server)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{Card: b.peerCard(t), Carrier: pair.Client})
			if err != nil {
				t.Fatal(err)
			}
			// a stream in use: a session with no streams drains at once and would hide the problem
			sink := &signalSink{}
			b.rt.CallTransport().RegisterFastFrameHandler(transport.Signal, sink.handler)
			aID, bID := types.PeerID(a.id.DeviceID), types.PeerID(b.id.DeviceID)
			mf, _ := signaling.EncodeSignal(signaling.ActionTreeManifest, "c1", aID, 1, &tree.TreeManifest{CallID: "c1", Epoch: 1, Coordinator: aID})
			if err := a.rt.CallTransport().SendRawFrame(bID, transport.Signal, mf); err != nil {
				t.Fatal(err)
			}
			sink.wait(t, 1)

			stop := a
			if stopper == "listener" {
				stop = b
			}
			sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer scancel()
			if err := stop.rt.Stop(sctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-sess.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("the session is still open 5 s after the %s runtime stopped", stopper)
			}
		})
	}
}
