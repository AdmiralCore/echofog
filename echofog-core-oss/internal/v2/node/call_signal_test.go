package node_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"sync"
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

type signalSink struct {
	mu  sync.Mutex
	got []*signaling.CallSignalEnvelope
}

func (s *signalSink) handler(p []byte, _ types.PeerID) error {
	env, err := signaling.DecodeSignal(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.got = append(s.got, env)
	s.mu.Unlock()
	return nil
}

func (s *signalSink) wait(t *testing.T, n int) []*signaling.CallSignalEnvelope {
	t.Helper()
	waitUntil(t, 5*time.Second, "signals delivered", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.got) >= n })
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*signaling.CallSignalEnvelope(nil), s.got...)
}

func (s *signalSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.got) }

// T9.3b: the call layer's signals travel as signed CallSignal over call/2, between real runtimes,
// and arrive as the v1 envelopes the call layer reads, with the sender the session proved.
func TestCallSignalsOverCall2(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	b.start(t)
	a.start(t)
	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, _, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{Card: b.peerCard(t), Carrier: pair.Client}); err != nil {
		t.Fatal(err)
	}

	ta, tb := a.rt.CallTransport(), b.rt.CallTransport()
	if a.rt.Call() == nil || ta == nil || tb == nil {
		t.Fatal("call/2 is not wired into the runtime")
	}
	sink := &signalSink{}
	tb.RegisterFastFrameHandler(transport.Signal, sink.handler)
	tb.RegisterFastFrameHandler(transport.SignalLossy, sink.handler)
	aID, bID := types.PeerID(a.id.DeviceID), types.PeerID(b.id.DeviceID)

	// a flooded signal and a join with the per-join media key
	mf, _ := signaling.EncodeSignal(signaling.ActionTreeManifest, "c1", aID, 1, &tree.TreeManifest{CallID: "c1", Epoch: 1, Coordinator: aID})
	if err := ta.SendRawFrame(bID, transport.Signal, mf); err != nil {
		t.Fatalf("send manifest: %v", err)
	}
	pub := []byte(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, 32)).Public().(ed25519.PublicKey))
	jn, _ := signaling.EncodeSignal(signaling.ActionJoin, "c1", aID, 1, signaling.JoinPayload{
		Proof: []byte{1}, KX: bytes.Repeat([]byte{8}, 32), SK: pub, JoinSalt: bytes.Repeat([]byte{6}, 16)})
	if err := ta.SendRawFrame(bID, transport.Signal, jn); err != nil {
		t.Fatalf("send join: %v", err)
	}
	got := sink.wait(t, 2)
	if got[0].Action != signaling.ActionTreeManifest || got[0].Sender != aID || got[1].Action != signaling.ActionJoin || got[1].Sender != aID {
		t.Fatalf("delivered %+v %+v", got[0], got[1])
	}

	// a signal in B's name from A is refused before it leaves
	forged, _ := signaling.EncodeSignal(signaling.ActionPing, "c1", bID, 1, nil)
	if err := ta.SendRawFrame(bID, transport.Signal, forged); err == nil {
		t.Fatal("A sent a signal in B's name")
	}
	time.Sleep(200 * time.Millisecond)
	if sink.count() != 2 {
		t.Fatalf("%d signals, want 2", sink.count())
	}
}
