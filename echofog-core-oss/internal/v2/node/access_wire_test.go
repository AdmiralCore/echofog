package node_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// ---- access listener harness

type accessCall struct {
	slot int
	keys [][]byte
}

type accessSlotListener struct {
	*carrierQueue
	mu     sync.Mutex
	closed bool
}

func (l *accessSlotListener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return l.carrierQueue.Close()
}

func (l *accessSlotListener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// accessFactory records every access listener the runtime installs. The listener has no
// in-place key update, so the runtime rebuilds it whenever the key set changes.
type accessFactory struct {
	mu     sync.Mutex
	calls  []accessCall
	bySlot map[int]*accessSlotListener
}

func newAccessFactory() *accessFactory {
	return &accessFactory{bySlot: make(map[int]*accessSlotListener)}
}

func (f *accessFactory) build(slot int, keys []node.AccessMaskKey) (carrier.Listener, error) {
	pubs := make([][]byte, 0, len(keys))
	for _, k := range keys {
		pubs = append(pubs, append([]byte(nil), k.Public...))
	}
	l := &accessSlotListener{carrierQueue: newCarrierQueue()}
	f.mu.Lock()
	f.calls = append(f.calls, accessCall{slot: slot, keys: pubs})
	f.bySlot[slot] = l
	f.mu.Unlock()
	return l, nil
}

func (f *accessFactory) latest(slot int) *accessSlotListener {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bySlot[slot]
}

func (f *accessFactory) lastKeys(slot int) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].slot == slot {
			return f.calls[i].keys
		}
	}
	return nil
}

// newHiddenAccessNode builds a hidden runtime whose passes live in the store and whose access
// listeners come from the factory.
func newHiddenAccessNode(t *testing.T, f node.AccessListenerFactory) (*testNode, *carrierQueue, *access.Manager) {
	return newHiddenAccessNodeCfg(t, f, nil)
}

func newHiddenAccessNodeCfg(t *testing.T, f node.AccessListenerFactory, mutate func(*node.Config, *carrierQueue)) (*testNode, *carrierQueue, *access.Manager) {
	t.Helper()

	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatalf("identity.NewThrowaway: %v", err)
	}
	devices, err := tw.DeviceList(1, time.Now())
	if err != nil {
		tw.Destroy()
		t.Fatalf("DeviceList: %v", err)
	}
	mask := tw.Device.Masking().Public()
	builder, err := cards.New(cards.Config{
		Account:  tw.Account.Public(),
		Device:   tw.Device,
		HsKey:    tw.Device.Handshake().Public(),
		L1Suites: []uint32{1, 2},
		Role:     objects.RoleUser,
		Listeners: []cards.Listener{{
			Suite: objects.SuiteNoise3,
			Host:  "127.0.0.1",
			Port:  4430,
			Mask:  mask,
		}},
		Devices:    devices,
		Visibility: cards.Hidden,
	})
	if err != nil {
		tw.Destroy()
		t.Fatalf("cards.New: %v", err)
	}

	st, err := store.Open(store.Options{Dir: t.TempDir(), Key: tw.Account.StorageKey()})
	if err != nil {
		tw.Destroy()
		t.Fatalf("store.Open: %v", err)
	}
	mgr, err := access.New(access.Config{
		KV:          st.Store(access.Namespace),
		Cards:       builder,
		ContactsKey: tw.Device.Masking(),
		Listeners: []cards.Listener{{
			Suite: objects.SuiteNoise3,
			Host:  "203.0.113.10",
			Port:  4242,
		}},
	})
	if err != nil {
		_ = st.Close()
		tw.Destroy()
		t.Fatalf("access.New: %v", err)
	}

	contacts := newCarrierQueue()
	cfg := node.Config{
		Account:        tw.Account,
		Device:         tw.Device,
		Cards:          builder,
		Store:          st,
		Access:         mgr,
		AccessListener: f,
		Listeners:      []carrier.Listener{contacts},
		Features:       []uint32{uint32(frame.FeatureCardPush)},
		Profile:        node.Profile{Platform: node.PlatformDesktop},
	}
	if mutate != nil {
		mutate(&cfg, contacts)
	}
	rt, err := node.New(cfg)
	if err != nil {
		mgr.Close()
		_ = st.Close()
		tw.Destroy()
		t.Fatalf("node.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Stop(ctx)
		mgr.Close()
		_ = st.Close()
		tw.Destroy()
	})

	return &testNode{
		account: tw.Account,
		device:  tw.Device,
		id: node.Identity{
			NodeID:   node.NodeID(tw.Account.NodeID()),
			DeviceID: node.DeviceID(tw.Device.ID()),
		},
		rt:      rt,
		store:   st,
		builder: builder,
	}, contacts, mgr
}

func newCardPushClient(t *testing.T) *testNode {
	t.Helper()
	n := newTestNodeWith(t, cards.Visible, true, nil, nil, func(c *node.Config) {
		c.Features = []uint32{uint32(frame.FeatureCardPush)}
	})
	n.start(t)
	return n
}

func hasMask(c *objects.Card, pub []byte) bool {
	for _, m := range c.Body.GetMaskKeys() {
		if bytes.Equal(m.GetKey(), pub) {
			return true
		}
	}
	return false
}

func hasPublicKey(keys [][]byte, pub []byte) bool {
	for _, k := range keys {
		if bytes.Equal(k, pub) {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func personalPass(t *testing.T, mgr *access.Manager) *access.Info {
	t.Helper()
	list, err := mgr.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := range list {
		if list[i].Kind == access.Personal && list[i].State == access.Active {
			return &list[i]
		}
	}
	return nil
}

// TestRuntimeAccessInvitationRedeemedAndContactReturns covers the T3.6b acceptance path on an
// in-memory carrier: the invitation is redeemed by the runtime, its L0 key disappears, the
// contact receives the peer card over the session and later enters with the personal pass and no
// link; revoking the contact then makes the handshake fail with the uniform silent close.
func TestRuntimeAccessInvitationRedeemedAndContactReturns(t *testing.T) {
	f := newAccessFactory()
	server, contacts, mgr := newHiddenAccessNode(t, f.build)
	server.start(t)
	client := newCardPushClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	minted, err := server.rt.MintInvitation(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("MintInvitation: %v", err)
	}
	if keys := f.lastKeys(minted.Listener); !hasPublicKey(keys, minted.MaskKey) {
		t.Fatalf("invitation key was not installed: %v", keys)
	}
	invite, err := objects.VerifyCard(minted.Card, time.Now())
	if err != nil {
		t.Fatalf("invitation card: %v", err)
	}

	first := memcarrier.NewQUIC()
	f.latest(minted.Listener).push(first.Server)
	_, sess1, err := client.rt.Session(ctx, server.id.DeviceID, node.DialOpt{
		Card: invite, Carrier: first.Client, AccessSecret: minted.Secret,
	})
	if err != nil {
		t.Fatalf("first session: %v", err)
	}

	var personal *access.Info
	waitFor(t, 10*time.Second, "invitation redemption", func() bool {
		personal = personalPass(t, mgr)
		return personal != nil
	})
	if len(personal.Secret) != 16 {
		t.Fatalf("personal pass secret is %d bytes", len(personal.Secret))
	}
	waitFor(t, 10*time.Second, "revoked invitation key removal", func() bool {
		l := f.latest(minted.Listener)
		return l == nil || l.isClosed()
	})

	var peerCard *objects.Card
	waitFor(t, 10*time.Second, "peer card delivered over the session", func() bool {
		peerCard = client.rt.Cards().Peer(server.id.DeviceID)
		return peerCard != nil
	})
	if peerCard.Scope != objects.ScopePeer {
		t.Fatalf("delivered card scope %v, want peer", peerCard.Scope)
	}

	// A contacts-key rotation reissues the peer card and pushes it to the live contact session.
	oldEnvelope := peerCard.Env.Raw
	rot, err := server.rt.RotateAccessKeys(ctx)
	if err != nil {
		t.Fatalf("RotateAccessKeys: %v", err)
	}
	if len(rot.Reissued) != 0 {
		t.Fatalf("%d reissued invitations, want none", len(rot.Reissued))
	}
	var rotated *objects.Card
	waitFor(t, 10*time.Second, "rotated peer card over CARD_UPDATE", func() bool {
		rotated = client.rt.Cards().Peer(server.id.DeviceID)
		return rotated != nil && !bytes.Equal(rotated.Env.Raw, oldEnvelope)
	})
	if !hasMask(rotated, rot.ContactsKey) {
		t.Fatal("the pushed peer card does not carry the new contacts key")
	}
	peerCard = rotated
	_ = sess1.Close()

	// The contact returns with the personal pass and no link, using the peer card from CARD_UPDATE.
	// A second runtime stands for the contact on another device, so the session table does not
	// see a duplicate of the still-draining first session.
	returning := newCardPushClient(t)
	second := memcarrier.NewQUIC()
	contacts.push(second.Server)
	_, sess2, err := returning.rt.Session(ctx, server.id.DeviceID, node.DialOpt{
		Card: peerCard, Carrier: second.Client, AccessSecret: personal.Secret,
	})
	if err != nil {
		t.Fatalf("second session with the personal pass: %v", err)
	}
	_ = sess2.Close()

	if _, err := server.rt.RevokeContactAccess(ctx, client.id.NodeID); err != nil {
		t.Fatalf("RevokeContactAccess: %v", err)
	}
	if personalPass(t, mgr) != nil {
		t.Fatal("revoked contact still has an active personal pass")
	}

	third := memcarrier.NewQUIC()
	contacts.push(third.Server)
	_, _, err = returning.rt.Session(ctx, server.id.DeviceID, node.DialOpt{
		Card: peerCard, Carrier: third.Client, AccessSecret: personal.Secret,
	})
	if err == nil {
		t.Fatal("a revoked contact completed a session")
	}
	if !errors.Is(err, session.ErrSilent) {
		t.Fatalf("revoked contact error %v, want a silent close", err)
	}
}

// TestRuntimeAccessRevokedInvitationUpdatesL0 checks that a revoked invitation's masking key is
// removed from the installed L0 listener as soon as the runtime operation returns, and that the
// listener of the slot disappears when no key is left.
func TestRuntimeAccessRevokedInvitationUpdatesL0(t *testing.T) {
	f := newAccessFactory()
	server, _, _ := newHiddenAccessNode(t, f.build)
	server.start(t)
	ctx := t.Context()

	first, err := server.rt.MintInvitation(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("MintInvitation: %v", err)
	}
	second, err := server.rt.MintInvitation(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("MintInvitation: %v", err)
	}
	if first.Listener != second.Listener {
		t.Fatalf("slots %d and %d, want one", first.Listener, second.Listener)
	}
	if keys := f.lastKeys(first.Listener); len(keys) != 2 {
		t.Fatalf("listener holds %d keys, want 2", len(keys))
	}

	if _, err := server.rt.RevokeAccess(ctx, first.PassID); err != nil {
		t.Fatalf("RevokeAccess: %v", err)
	}
	keys := f.lastKeys(first.Listener)
	if hasPublicKey(keys, first.MaskKey) {
		t.Fatal("revoked invitation key is still installed at L0")
	}
	if !hasPublicKey(keys, second.MaskKey) {
		t.Fatal("the remaining invitation key was lost")
	}

	if _, err := server.rt.RevokeAccess(ctx, second.PassID); err != nil {
		t.Fatalf("RevokeAccess: %v", err)
	}
	if l := f.latest(first.Listener); l != nil && !l.isClosed() {
		t.Fatal("the listener of a slot with no invitation keys is still open")
	}
}

// TestRuntimeAccessRotateKeysUpdatesBuilder checks the contacts-key rotation: the card builder
// switches to the new key, the next peer card carries it, and the reissued invitations are
// returned to the owner for delivery.
func TestRuntimeAccessRotateKeysUpdatesBuilder(t *testing.T) {
	f := newAccessFactory()
	server, _, _ := newHiddenAccessNode(t, f.build)
	server.start(t)
	ctx := t.Context()

	minted, err := server.rt.MintInvitation(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("MintInvitation: %v", err)
	}
	before, err := server.builder.Current(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("peer card before rotation: %v", err)
	}

	rot, err := server.rt.RotateAccessKeys(ctx)
	if err != nil {
		t.Fatalf("RotateAccessKeys: %v", err)
	}
	if len(rot.Reissued) != 1 {
		t.Fatalf("%d reissued invitations, want 1", len(rot.Reissued))
	}
	if rot.Reissued[0].PassID == minted.PassID {
		t.Fatal("reissued invitation kept the old pass id")
	}

	after, err := server.builder.Current(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatalf("peer card after rotation: %v", err)
	}
	if bytes.Equal(before.Envelope, after.Envelope) {
		t.Fatal("contacts-key rotation did not reissue the peer card")
	}
	card, err := objects.VerifyCard(after.Envelope, time.Now())
	if err != nil {
		t.Fatalf("peer card after rotation: %v", err)
	}
	if !hasMask(card, rot.ContactsKey) {
		t.Fatal("the new peer card does not carry the new contacts key")
	}
	if keys := f.lastKeys(minted.Listener); !hasPublicKey(keys, rot.Reissued[0].MaskKey) {
		t.Fatal("the invitation listener still has the old key")
	}
}
