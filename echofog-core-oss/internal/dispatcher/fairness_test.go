package dispatcher

import (
	"context"
	"crypto/ed25519"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/abuse"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

type sender struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	id   types.PeerID
}

func newSender(t *testing.T) sender {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return sender{pub, priv, types.PeerPubKeyToID(types.PeerPublicKey(pub))}
}

func (s sender) env(nonce int64) *internal_pb.Envelope {
	data, _ := proto.Marshal(&internal_pb.MessageData{
		MessageId: []byte("m"),
		Payload:   &internal_pb.MessageData_Ping{Ping: &internal_pb.Ping{Nonce: nonce}},
	})
	return &internal_pb.Envelope{Data: data, Signature: ed25519.Sign(s.priv, data), PubKey: s.pub}
}

type reports struct {
	mu  sync.Mutex
	got map[types.PeerID][]abuse.Reason
}

func (r *reports) ReportPeer(id types.PeerID, why abuse.Reason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.got == nil {
		r.got = make(map[types.PeerID][]abuse.Reason)
	}
	r.got[id] = append(r.got[id], why)
}

func (r *reports) of(id types.PeerID) []abuse.Reason {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]abuse.Reason(nil), r.got[id]...)
}

// One peer that floods the queue cannot keep another peer's messages out: it gets only its share.
func TestDispatcher_PeerCannotFillTheQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewDispatcher(ctx)
	rep := &reports{}
	d.SetReporter(rep)

	honestSeen := make(chan struct{}, 1)
	flooder, honest := newSender(t), newSender(t)
	d.Registry(&internal_pb.MessageData_Ping{}, &mockHandler{onMsg: func(msg *internal_pb.MessageData, from types.PeerID) {
		if from == honest.id {
			select {
			case honestSeen <- struct{}{}:
			default:
			}
			return
		}
		time.Sleep(2 * time.Millisecond) // the flooder's messages are slow to handle: the queue backs up
	}})
	d.Start()
	defer d.Stop()

	// the flooder pushes far faster than its messages are handled, and keeps on doing so
	stop := make(chan struct{})
	defer close(stop)
	env := flooder.env(1)
	for i := 0; i < queueSize*2; i++ {
		d.PushMessage(env, flooder.id)
	}
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				d.PushMessage(env, flooder.id)
			}
		}
	}()
	share, full := d.Dropped()
	if share == 0 {
		t.Fatal("the flooder should have been held to its share of the queue")
	}
	if full != 0 {
		t.Fatalf("the queue itself must not fill up from one peer, %d dropped as full", full)
	}
	d.queuedMu.Lock()
	held := d.queued[flooder.id]
	d.queuedMu.Unlock()
	if held > d.perPeer {
		t.Fatalf("flooder holds %d of the queue, its share is %d", held, d.perPeer)
	}

	start := time.Now()
	d.PushMessage(honest.env(2), honest.id)
	select {
	case <-honestSeen:
		t.Logf("honest message handled %v after it came, during the flood", time.Since(start))
	case <-time.After(3 * time.Second):
		t.Fatal("an honest peer's message did not get through the flood")
	}
	found := false
	for _, r := range rep.of(flooder.id) {
		found = found || r == abuse.Flood
	}
	if !found {
		t.Fatal("the flood was not reported")
	}
	if len(rep.of(honest.id)) != 0 {
		t.Fatal("the honest peer was reported")
	}
}

// A bad signature is the sender's fault only when it signed the envelope itself; one it forwards
// for somebody else (a relay) is not held against it.
func TestDispatcher_ReportsOnlyOwnBadEnvelopes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewDispatcher(ctx)
	rep := &reports{}
	d.SetReporter(rep)
	d.Registry(&internal_pb.MessageData_Ping{}, &mockHandler{})
	d.Start()
	defer d.Stop()

	liar, relay, origin := newSender(t), newSender(t), newSender(t)
	bad := liar.env(1)
	bad.Signature[0] ^= 0xff
	d.PushMessage(bad, liar.id)

	forwarded := origin.env(2)
	forwarded.Signature[0] ^= 0xff
	d.PushMessage(forwarded, relay.id)

	deadline := time.Now().Add(2 * time.Second)
	for len(rep.of(liar.id)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := rep.of(liar.id); len(got) != 1 || got[0] != abuse.BadSignature {
		t.Fatalf("want one bad-signature report for the signer, got %v", got)
	}
	if got := rep.of(relay.id); len(got) != 0 {
		t.Fatalf("the relay forwarded somebody else's envelope and must not be blamed, got %v", got)
	}
}
