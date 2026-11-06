package sigbridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/internal/v2/call"
	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/call/v1sig"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type fakeSvc struct {
	mu      sync.Mutex
	sent    []*pb.CallSignal
	sentTo  [][32]byte
	fwd     [][]byte
	fwdTo   [][32]byte
	sendErr error
}

func (f *fakeSvc) Send(_ context.Context, p call.Peer, m *pb.CallSignal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent, f.sentTo = append(f.sent, m), append(f.sentTo, p.Device)
	return f.sendErr
}

func (f *fakeSvc) Forward(_ context.Context, p call.Peer, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fwd, f.fwdTo = append(f.fwd, append([]byte(nil), raw...)), append(f.fwdTo, p.Device)
	return nil
}

type delivered struct {
	from [32]byte
	env  *signaling.CallSignalEnvelope
}

type rig struct {
	b    *Bridge
	svc  *fakeSvc
	self types.PeerID
	got  chan delivered
	now  time.Time
}

func pid(b byte) types.PeerID { return types.PeerID{b, b, b} }

func newRig(t *testing.T, guard bool) *rig {
	t.Helper()
	r := &rig{svc: &fakeSvc{}, self: pid(1), got: make(chan delivered, 16), now: time.UnixMilli(1_700_000_000_000)}
	cfg := Config{
		Self: r.self, Service: r.svc, Context: t.Context(), Now: func() time.Time { return r.now },
		Deliver: func(from [32]byte, v1 []byte) {
			env, err := signaling.DecodeSignal(v1)
			if err != nil {
				t.Errorf("delivered garbage: %v", err)
				return
			}
			r.got <- delivered{from, env}
		},
	}
	if guard {
		cfg.Guard = call.NewJoinGuard(call.JoinGuardConfig{
			Now: func() time.Time { return r.now }, After: func(time.Duration) <-chan time.Time { c := make(chan time.Time, 1); c <- r.now; return c },
		})
	}
	r.b = New(cfg)
	return r
}

func v1env(t *testing.T, sender types.PeerID, a signaling.SignalAction, p any) []byte {
	t.Helper()
	b, err := signaling.EncodeSignal(a, "c1", sender, 1, p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mkManifest() *tree.TreeManifest {
	return &tree.TreeManifest{CallID: "c1", Epoch: 1, Coordinator: pid(2), Timestamp: 5}
}

func joinPayload(salt byte) signaling.JoinPayload {
	pub := []byte(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{salt}, 32)).Public().(ed25519.PublicKey))
	return signaling.JoinPayload{Proof: []byte{1}, KX: bytes.Repeat([]byte{8}, 32), SK: pub, JoinSalt: bytes.Repeat([]byte{salt}, 16)}
}

func (r *rig) inbound(t *testing.T, from types.PeerID, m *pb.CallSignal) []byte {
	t.Helper()
	raw, err := call.Encode(m, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.b.OnSignal(t.Context(), call.Peer{Device: from}, m, raw)
	return raw
}

func (r *rig) take(t *testing.T) (delivered, bool) {
	t.Helper()
	select {
	case d := <-r.got:
		return d, true
	case <-time.After(300 * time.Millisecond):
		return delivered{}, false
	}
}

func TestOwnSignalIsSentAsCallSignal(t *testing.T) {
	r := newRig(t, false)
	if err := r.b.SendSignal(pid(2), false, v1env(t, r.self, signaling.ActionTreeManifest, mkManifest())); err != nil {
		t.Fatal(err)
	}
	if len(r.svc.sent) != 1 || r.svc.sentTo[0] != pid(2) || r.svc.sent[0].Action != pb.Action_ACTION_TREE_MANIFEST || len(r.svc.fwd) != 0 {
		t.Fatalf("sent %+v forwarded %d", r.svc.sent, len(r.svc.fwd))
	}
	if r.svc.sent[0].MsgId == 0 || r.svc.sent[0].Timestamp == 0 {
		t.Fatalf("the v1 message id and time must survive: %+v", r.svc.sent[0])
	}
	// the lossy copy of a signal is the service's business
	if err := r.b.SendSignal(pid(2), true, v1env(t, r.self, signaling.ActionPing, nil)); err != nil || len(r.svc.sent) != 1 {
		t.Fatalf("lossy copy sent: %v %d", err, len(r.svc.sent))
	}
}

func TestFloodedSignalIsForwardedByteForByte(t *testing.T) {
	r := newRig(t, false)
	origin := &pb.CallSignal{Action: pb.Action_ACTION_TREE_MANIFEST, CallId: "c1", Sender: idb(pid(2)), MsgId: 77, Epoch: 1, Timestamp: 5,
		Payload: &pb.CallSignal_TreeManifest{TreeManifest: &pb.TreeManifest{CallId: "c1", Epoch: 1, Coordinator: idb(pid(2))}}}
	raw := r.inbound(t, pid(3), origin)
	d, ok := r.take(t)
	if !ok || d.from != pid(3) || d.env.Sender != pid(2) || d.env.MsgID != 77 {
		t.Fatalf("delivered %+v ok=%v", d, ok)
	}
	// The call layer floods the envelope on to another neighbour, as it received it.
	v1, _ := v1sig.ToV1(origin)
	if err := r.b.SendSignal(pid(4), false, v1); err != nil {
		t.Fatal(err)
	}
	if len(r.svc.fwd) != 1 || !bytes.Equal(r.svc.fwd[0], raw) || r.svc.fwdTo[0] != pid(4) || len(r.svc.sent) != 0 {
		t.Fatalf("forwarded %d sent %d: the wrapper must go on unchanged", len(r.svc.fwd), len(r.svc.sent))
	}
}

func TestSignalOfAnotherSenderWithoutItsWrapperIsRefused(t *testing.T) {
	r := newRig(t, false)
	err := r.b.SendSignal(pid(4), false, v1env(t, pid(2), signaling.ActionTreeManifest, mkManifest()))
	if err == nil {
		t.Fatal("a signal in another peer's name was sent")
	}
	if len(r.svc.sent)+len(r.svc.fwd) != 0 {
		t.Fatal("something left")
	}
	// unmappable payloads never leave either
	if err := r.b.SendSignal(pid(4), false, v1env(t, r.self, signaling.ActionTreeManifest, "text")); !errors.Is(err, v1sig.ErrMapping) {
		t.Fatalf("bad payload: %v", err)
	}
	if err := r.b.SendSignal(pid(4), false, []byte("{")); err == nil {
		t.Fatal("garbage sent")
	}
}

func joinSignal(sender types.PeerID, a pb.Action, salt byte, ts int64) *pb.CallSignal {
	p := joinPayload(salt)
	return &pb.CallSignal{Action: a, CallId: "c1", Sender: idb(sender), MsgId: uint64(salt), Timestamp: ts,
		Payload: &pb.CallSignal_Join{Join: &pb.Join{Proof: p.Proof, Kx: p.KX, MediaKey: p.SK, JoinSalt: p.JoinSalt}}}
}

func TestJoinsGoThroughTheGuard(t *testing.T) {
	r := newRig(t, true)
	ts := r.now.UnixMilli()
	r.inbound(t, pid(5), joinSignal(pid(5), pb.Action_ACTION_JOIN, 9, ts))
	if d, ok := r.take(t); !ok || d.env.Action != signaling.ActionJoin {
		t.Fatalf("first join not delivered: %+v %v", d, ok)
	}
	// a replay of the same join (same salt) is refused, silently
	r.inbound(t, pid(5), joinSignal(pid(5), pb.Action_ACTION_JOIN, 9, ts))
	if d, ok := r.take(t); ok {
		t.Fatalf("a replayed join was delivered: %+v", d)
	}
	// the member heartbeat carries the same join: passes for an admitted pair...
	r.inbound(t, pid(5), joinSignal(pid(5), pb.Action_ACTION_MEMBER_HEARTBEAT, 9, ts))
	if _, ok := r.take(t); !ok {
		t.Fatal("heartbeat of an admitted member dropped")
	}
	// ...and never admits a pair the guard has not seen
	r.inbound(t, pid(6), joinSignal(pid(6), pb.Action_ACTION_MEMBER_HEARTBEAT, 3, ts))
	if d, ok := r.take(t); ok {
		t.Fatalf("a heartbeat admitted a member without a join: %+v", d)
	}
	// a stale join is refused
	r.inbound(t, pid(7), joinSignal(pid(7), pb.Action_ACTION_JOIN, 4, ts-10*60*1000))
	if d, ok := r.take(t); ok {
		t.Fatalf("a stale join was delivered: %+v", d)
	}
}

func TestInboundOfAnotherSenderIsNotTrustedFromThePayload(t *testing.T) {
	r := newRig(t, false)
	// the call layer must see the verified sender, which Process checked; a CallSignal that cannot
	// be mapped is dropped quietly
	r.inbound(t, pid(3), &pb.CallSignal{Action: pb.Action_ACTION_TREE_MANIFEST, CallId: "c1", Sender: idb(pid(2)), MsgId: 1})
	if d, ok := r.take(t); ok {
		t.Fatalf("an unmappable signal was delivered: %+v", d)
	}
}

func idb(p types.PeerID) []byte { return append([]byte(nil), p[:]...) }
