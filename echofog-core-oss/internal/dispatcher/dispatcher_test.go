package dispatcher

import (
	"context"
	"crypto/ed25519"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

type mockHandler struct {
	handled atomic.Int32
	onMsg   func(msg *internal_pb.MessageData, peerID types.PeerID)
}

func (m *mockHandler) Handle(msg *internal_pb.MessageData, peerID types.PeerID) {
	m.handled.Add(1)
	if m.onMsg != nil {
		m.onMsg(msg, peerID)
	}
}

func TestDispatcher_LifecycleAndRouting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := NewDispatcher(ctx)
	d.Start()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	peerID := types.PeerPubKeyToID(types.PeerPublicKey(pub))

	var receivedPing atomic.Bool
	handler := &mockHandler{
		onMsg: func(msg *internal_pb.MessageData, pID types.PeerID) {
			if _, ok := msg.Payload.(*internal_pb.MessageData_Ping); ok {
				receivedPing.Store(true)
			}
		},
	}

	d.Registry(&internal_pb.MessageData_Ping{}, handler)

	msgData := &internal_pb.MessageData{
		MessageId: []byte("test-msg-id"),
		Payload: &internal_pb.MessageData_Ping{
			Ping: &internal_pb.Ping{Nonce: 12345},
		},
	}
	dataBytes, err := proto.Marshal(msgData)
	if err != nil {
		t.Fatal(err)
	}

	sig := ed25519.Sign(priv, dataBytes)
	env := &internal_pb.Envelope{
		Data:      dataBytes,
		Signature: sig,
		PubKey:    pub,
	}

	d.PushMessage(env, peerID)

	// Wait for delivery
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if receivedPing.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !receivedPing.Load() {
		t.Fatalf("expected message to be routed to registered handler")
	}

	// Verify Stop completes cleanly without leaking workers
	d.Stop()
}

func TestDispatcher_PanicShield(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := NewDispatcher(ctx)
	d.Start()
	defer d.Stop()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	peerID := types.PeerPubKeyToID(types.PeerPublicKey(pub))

	var normalHandled atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)

	// Handler that panics on first message, succeeds on second
	var count atomic.Int32
	handler := &mockHandler{
		onMsg: func(msg *internal_pb.MessageData, pID types.PeerID) {
			c := count.Add(1)
			if c == 1 {
				panic("simulated panic in application handler")
			}
			normalHandled.Store(true)
			wg.Done()
		},
	}

	d.Registry(&internal_pb.MessageData_Ping{}, handler)

	makeEnv := func(nonce int64) *internal_pb.Envelope {
		msgData := &internal_pb.MessageData{
			MessageId: []byte("test-msg"),
			Payload:   &internal_pb.MessageData_Ping{Ping: &internal_pb.Ping{Nonce: nonce}},
		}
		dataBytes, _ := proto.Marshal(msgData)
		sig := ed25519.Sign(priv, dataBytes)
		return &internal_pb.Envelope{
			Data:      dataBytes,
			Signature: sig,
			PubKey:    pub,
		}
	}

	// Push panicking message
	d.PushMessage(makeEnv(1), peerID)
	time.Sleep(50 * time.Millisecond)

	// Push second message — worker should still be alive due to panic shield!
	d.PushMessage(makeEnv(2), peerID)

	wg.Wait()
	if !normalHandled.Load() {
		t.Fatalf("expected second message to be processed after first message panicked")
	}
}

// A message may not name an origin other than the key that signed it.
func TestDispatcher_DropsForgedOrigin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewDispatcher(ctx)
	d.Start()

	pub, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	self := types.PeerPubKeyToID(types.PeerPublicKey(pub))
	victim := types.PeerPubKeyToID(types.PeerPublicKey(otherPub))

	handler := &mockHandler{}
	d.Registry(&internal_pb.MessageData_Ping{}, handler)

	push := func(origin types.PeerID) {
		data, _ := proto.Marshal(&internal_pb.MessageData{
			MessageId: []byte("m"),
			OriginId:  origin[:],
			Payload:   &internal_pb.MessageData_Ping{Ping: &internal_pb.Ping{Nonce: 1}},
		})
		d.PushMessage(&internal_pb.Envelope{Data: data, Signature: ed25519.Sign(priv, data), PubKey: pub}, self)
	}
	push(victim)
	push(self)

	deadline := time.Now().Add(2 * time.Second)
	for handler.handled.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := handler.handled.Load(); n != 1 {
		t.Fatalf("want only the honest message handled, got %d", n)
	}
}
