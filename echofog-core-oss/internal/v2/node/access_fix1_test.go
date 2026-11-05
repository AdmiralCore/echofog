package node_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"

	"golang.org/x/crypto/curve25519"
)

// redeemContact runs the first session of a contact through an invitation and returns the
// contact runtime, its live session, the peer card it holds and the personal pass.
func redeemContact(t *testing.T, ctx context.Context, server *testNode, mgr *access.Manager, f *accessFactory) (*testNode, *mux.Session, *objects.Card, *access.Info) {
	t.Helper()
	client := newCardPushClient(t)
	minted, err := server.rt.MintInvitation(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("MintInvitation: %v", err)
	}
	invite, err := objects.VerifyCard(minted.Card, time.Now())
	if err != nil {
		t.Fatalf("invitation card: %v", err)
	}
	c := memcarrier.NewQUIC()
	f.latest(minted.Listener).push(c.Server)
	_, sess, err := client.rt.Session(ctx, server.id.DeviceID, node.DialOpt{
		Card: invite, Carrier: c.Client, AccessSecret: minted.Secret,
	})
	if err != nil {
		t.Fatalf("contact session: %v", err)
	}
	var personal *access.Info
	waitFor(t, 10*time.Second, "invitation redemption", func() bool {
		personal = personalPass(t, mgr)
		return personal != nil
	})
	var peer *objects.Card
	waitFor(t, 10*time.Second, "peer card delivered", func() bool {
		peer = client.rt.Cards().Peer(server.id.DeviceID)
		return peer != nil
	})
	return client, sess, peer, personal
}

// N1: a revoked contact with a live session must not receive the rotated card (it carries the
// key it was revoked to rotate away), and its session is closed.
func TestAccessRevokedContactLiveSessionGetsNoCardAndIsClosed(t *testing.T) {
	f := newAccessFactory()
	server, _, mgr := newHiddenAccessNode(t, f.build)
	server.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	client, sess, peer, _ := redeemContact(t, ctx, server, mgr, f)
	before := peer.Env.Raw

	if _, err := server.rt.RevokeContactAccess(ctx, client.id.NodeID); err != nil {
		t.Fatalf("RevokeContactAccess: %v", err)
	}
	_ = sess
	waitFor(t, 5*time.Second, "the revoked contact's session to close on the node", func() bool {
		return node.OpenSessions(server.rt) == 0
	})
	time.Sleep(200 * time.Millisecond)
	if got := client.rt.Cards().Peer(server.id.DeviceID); got == nil || !bytes.Equal(got.Env.Raw, before) {
		t.Fatal("the revoked contact received the rotated peer card")
	}
}

// N1: an outgoing session of the hidden node to a public node is in the same table; it must not
// get the peer card (contacts key and hidden listener address).
func TestAccessRotationCardNotPushedToOutgoingSessions(t *testing.T) {
	f := newAccessFactory()
	server, _, mgr := newHiddenAccessNode(t, f.build)
	server.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	q := newCarrierQueue()
	pub := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{q}, nil, func(c *node.Config) {
		c.Features = []uint32{uint32(frame.FeatureCardPush)}
	})
	pub.start(t)
	c := memcarrier.NewQUIC()
	q.push(c.Server)
	if _, _, err := server.rt.Session(ctx, pub.id.DeviceID, node.DialOpt{Card: pub.publicCard(t), Carrier: c.Client}); err != nil {
		t.Fatalf("outgoing session: %v", err)
	}

	redeemContact(t, ctx, server, mgr, f)
	if _, err := server.rt.RotateAccessKeys(ctx); err != nil {
		t.Fatalf("RotateAccessKeys: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := pub.rt.Cards().Peer(server.id.DeviceID); got != nil {
		t.Fatal("a public node received the hidden node's peer card")
	}
}

// contactListener is a contacts-key listener that records in-place key updates.
type contactListener struct {
	*carrierQueue
	mu   sync.Mutex
	sets [][]noise3.MaskKey
}

func (l *contactListener) SetKeys(keys []noise3.MaskKey) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sets = append(l.sets, append([]noise3.MaskKey(nil), keys...))
	return nil
}

func (l *contactListener) last() []noise3.MaskKey {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.sets) == 0 {
		return nil
	}
	return l.sets[len(l.sets)-1]
}

func publicOfMask(t *testing.T, k noise3.MaskKey) []byte {
	t.Helper()
	pub, err := curve25519.X25519(k.Private[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("X25519: %v", err)
	}
	return pub
}

// hasAKey reports whether the raw X25519 public keys contain the typed key (one type byte, then 32).
func hasAKey(raw [][]byte, akey []byte) bool {
	if len(akey) != 33 {
		return false
	}
	return hasPublicKey(raw, akey[1:])
}

// N3: the rotation reaches the contacts listener: the new key plus the previous one until its
// RetireAfter; the revoked-away key is gone once it retires.
func TestAccessRotationReachesContactListener(t *testing.T) {
	f := newAccessFactory()
	cl := &contactListener{carrierQueue: newCarrierQueue()}
	server, _, mgr := newHiddenAccessNodeCfg(t, f.build, func(c *node.Config, q *carrierQueue) {
		c.Listeners = nil
		c.ContactListeners = []carrier.Listener{cl}
	})
	server.start(t)
	ctx := t.Context()

	// A contact with a pass exists, so the previous key stays valid during the overlap.
	redeemContact(t, ctx, server, mgr, f)
	rot, err := server.rt.RotateAccessKeys(ctx)
	if err != nil {
		t.Fatalf("RotateAccessKeys: %v", err)
	}
	pubsOf := func() [][]byte {
		var pubs [][]byte
		for _, k := range cl.last() {
			pubs = append(pubs, publicOfMask(t, k))
		}
		return pubs
	}
	waitFor(t, 5*time.Second, "the new contacts key at the contacts listener", func() bool {
		return hasAKey(pubsOf(), rot.ContactsKey)
	})
	keys := cl.last()
	pubs := pubsOf()
	if rot.PreviousKey == nil {
		t.Fatal("no previous contacts key during the overlap")
	}
	{
		if !hasAKey(pubs, rot.PreviousKey) {
			t.Fatal("the previous contacts key is not kept during the overlap")
		}
		for _, k := range keys {
			if len(rot.PreviousKey) == 33 && bytes.Equal(publicOfMask(t, k), rot.PreviousKey[1:]) && k.NotAfter.IsZero() {
				t.Fatal("the previous contacts key has no deadline")
			}
		}
	}
}

// N4(a): the owner receives the reissued link only after the new key is installed at L0, and
// still receives it when the refresh fails.
func TestAccessReissuedDeliveredAfterRefreshEvenOnError(t *testing.T) {
	f := newAccessFactory()
	var failBuild atomic.Bool
	var installedAtDelivery atomic.Bool
	var delivered atomic.Int32
	var server *testNode
	var mgr *access.Manager
	factory := func(slot int, keys []node.AccessMaskKey) (carrier.Listener, error) {
		if failBuild.Load() {
			return nil, errors.New("listener build failed")
		}
		return f.build(slot, keys)
	}
	server, _, mgr = newHiddenAccessNodeCfg(t, factory, func(c *node.Config, q *carrierQueue) {
		c.AccessReissued = func(list []access.Minted) {
			delivered.Add(1)
			installedAtDelivery.Store(hasPublicKey(f.lastKeys(list[0].Listener), list[0].MaskKey))
		}
	})
	_ = mgr
	server.start(t)
	ctx := t.Context()

	if _, err := server.rt.MintInvitation(ctx, access.MintOptions{}); err != nil {
		t.Fatalf("MintInvitation: %v", err)
	}
	if _, err := server.rt.RotateAccessKeys(ctx); err != nil {
		t.Fatalf("RotateAccessKeys: %v", err)
	}
	if delivered.Load() != 1 || !installedAtDelivery.Load() {
		t.Fatalf("delivered=%d installedAtDelivery=%v: the link was handed out before L0 had its key",
			delivered.Load(), installedAtDelivery.Load())
	}

	failBuild.Store(true)
	if _, err := server.rt.RotateAccessKeys(ctx); err == nil {
		t.Fatal("expected a refresh error")
	}
	if delivered.Load() != 2 {
		t.Fatalf("delivered=%d after a failed refresh, want 2", delivered.Load())
	}
}

// N4(b): a slow listener refresh (it holds the access lock) must not stop the accept loop from
// taking the next contact session.
func TestAccessSlowRefreshDoesNotBlockAcceptLoop(t *testing.T) {
	f := newAccessFactory()
	var block atomic.Bool
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	factory := func(slot int, keys []node.AccessMaskKey) (carrier.Listener, error) {
		if block.Load() {
			<-release
		}
		return f.build(slot, keys)
	}
	server, contacts, mgr := newHiddenAccessNode(t, factory)
	server.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()

	_, sess1, peer, personal := redeemContact(t, ctx, server, mgr, f)
	_ = sess1.Close()
	if _, err := server.rt.MintInvitation(ctx, access.MintOptions{}); err != nil {
		t.Fatalf("MintInvitation: %v", err)
	}

	block.Store(true)
	rotDone := make(chan struct{})
	go func() {
		defer close(rotDone)
		_, _ = server.rt.RotateAccessKeys(ctx)
	}()
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 2; i++ {
		c := newCardPushClient(t)
		cc := memcarrier.NewQUIC()
		contacts.push(cc.Server)
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		_, s, err := c.rt.Session(dctx, server.id.DeviceID, node.DialOpt{
			Card: peer, Carrier: cc.Client, AccessSecret: personal.Secret,
		})
		dcancel()
		if err != nil {
			t.Fatalf("contact session %d while a refresh is slow: %v", i, err)
		}
		_ = s
	}
	free()
	<-rotDone
}

// N4(c): the pass is booked before the session enters the table, so a refused pass never gets to
// evict a live session by the duplicate rule.
func TestAccessAdmissionBeforeTableAdd(t *testing.T) {
	f := newAccessFactory()
	server, _, mgr := newHiddenAccessNode(t, f.build)
	server.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	var sawPass atomic.Bool
	node.SetBeforeAdd(func(est *session.Established) {
		if est.Access != nil && personalPass(t, mgr) != nil {
			sawPass.Store(true)
		}
	})
	t.Cleanup(func() { node.SetBeforeAdd(nil) })

	redeemContact(t, ctx, server, mgr, f)
	if !sawPass.Load() {
		t.Fatal("the session entered the table before its invitation was redeemed")
	}
}
