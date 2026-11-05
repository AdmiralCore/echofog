package node_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// ---- test identities

type testNode struct {
	account *identity.Account
	device  *identity.Device
	id      node.Identity
	rt      *node.Runtime
	store   *store.Store
}

func newTestNode(t *testing.T, visibility cards.Visibility, cover bool, listeners []carrier.Listener, events node.EventSink) *testNode {
	return newTestNodeWith(t, visibility, cover, listeners, events, nil)
}

func newTestNodeWith(t *testing.T, visibility cards.Visibility, cover bool, listeners []carrier.Listener, events node.EventSink, mutate func(*node.Config)) *testNode {
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
	cardListeners := []cards.Listener{{
		Suite: objects.SuiteNoise3,
		Host:  "127.0.0.1",
		Port:  4430,
		Mask:  mask,
	}}
	if cover {
		cardListeners = append(cardListeners, cards.Listener{
			Suite: objects.SuiteReality2QUIC,
			Host:  "203.0.113.10",
			Port:  443,
			SNI:   "example.com",
			Mask:  mask,
		})
	}

	builder, err := cards.New(cards.Config{
		Account:    tw.Account.Public(),
		Device:     tw.Device,
		HsKey:      tw.Device.Handshake().Public(),
		L1Suites:   []uint32{uint32(1), uint32(2)},
		Role:       objects.RoleUser,
		Listeners:  cardListeners,
		Devices:    devices,
		Visibility: visibility,
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

	cfg := node.Config{
		Account:   tw.Account,
		Device:    tw.Device,
		Cards:     builder,
		Store:     st,
		Listeners: listeners,
		Events:    events,
		Profile:   node.Profile{Platform: "test"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	rt, err := node.New(cfg)
	if err != nil {
		_ = st.Close()
		tw.Destroy()
		t.Fatalf("node.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Stop(ctx)
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
		rt:    rt,
		store: st,
	}
}

func (n *testNode) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.rt.Start(ctx); err != nil {
		t.Fatalf("Runtime.Start: %v", err)
	}
}

func (n *testNode) peerCard(t *testing.T) *objects.Card {
	t.Helper()
	env, ok := n.rt.Cards().Own(objects.ScopePeer)
	if !ok {
		t.Fatal("node has no peer card")
	}
	card, err := objects.VerifyCard(env, time.Now())
	if err != nil {
		t.Fatalf("verify own peer card: %v", err)
	}
	return card
}

func (n *testNode) publicCard(t *testing.T) *objects.Card {
	t.Helper()
	env, ok := n.rt.Cards().Own(objects.ScopePublic)
	if !ok {
		t.Fatal("node has no public card")
	}
	card, err := objects.VerifyCard(env, time.Now())
	if err != nil {
		t.Fatalf("verify own public card: %v", err)
	}
	return card
}

// ---- in-memory carrier listener

type carrierQueue struct {
	ch     chan mux.Carrier
	done   chan struct{}
	once   sync.Once
	closed error
}

func newCarrierQueue() *carrierQueue {
	return &carrierQueue{ch: make(chan mux.Carrier, 1), done: make(chan struct{})}
}

func (q *carrierQueue) push(c mux.Carrier) { q.ch <- c }

func (q *carrierQueue) Accept(ctx context.Context) (mux.Carrier, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.done:
		return nil, q.closed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (q *carrierQueue) Addr() net.Addr { return nil }

func (q *carrierQueue) Close() error {
	q.once.Do(func() {
		q.closed = errors.New("carrier queue closed")
		close(q.done)
	})
	return nil
}

// ---- fake service module

type fakeModule struct {
	got  chan node.Peer
	errs chan error
}

func (m *fakeModule) handler(_ context.Context, p node.Peer, st *mux.Stream) {
	m.got <- p
	f, err := st.ReadFrame()
	if err != nil {
		m.errs <- err
		_ = st.Close()
		return
	}
	if f.Type != 0x40 {
		m.errs <- errors.New("unexpected frame type")
		_ = st.Close()
		return
	}
	if err := st.WriteFrame(0x41, 0, []byte("pong")); err != nil {
		m.errs <- err
	}
	_ = st.Close()
}

func registerFakeModule(t *testing.T, rt *node.Runtime, m *fakeModule) {
	t.Helper()
	spec, ok := service.Builtin(service.Chat)
	if !ok {
		t.Fatal("chat service is not built in")
	}
	rt.Register(spec, m.handler)
}

// TestFakeModuleSeesPeerOverTwoRuntimes is the T3.7a acceptance test: two runtimes on an
// in-memory carrier negotiate a service, exchange a stream, and the handler sees the verified
// peer identity, session id and path.
func TestFakeModuleSeesPeerOverTwoRuntimes(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)

	mod := &fakeModule{got: make(chan node.Peer, 1), errs: make(chan error, 1)}
	registerFakeModule(t, a.rt, mod)
	registerFakeModule(t, b.rt, mod)

	b.start(t)
	a.start(t)

	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	peerOnA, stream, err := a.rt.Open(ctx, b.id.DeviceID, service.Chat, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: pair.Client,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if peerOnA.Device != b.id.DeviceID {
		t.Fatalf("initiator sees device %x, want %x", peerOnA.Device, b.id.DeviceID)
	}
	if peerOnA.Node != b.id.NodeID {
		t.Fatalf("initiator sees node %x, want %x", peerOnA.Node, b.id.NodeID)
	}
	if peerOnA.Card == nil || peerOnA.Card.DeviceID != [32]byte(b.id.DeviceID) {
		t.Fatal("initiator has no verified peer card")
	}
	if peerOnA.Path != mux.PathDirect {
		t.Fatalf("path %v, want direct", peerOnA.Path)
	}

	if err := stream.WriteFrame(0x40, 0, []byte("ping")); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	reply, err := stream.ReadFrame()
	if err != nil {
		select {
		case modErr := <-mod.errs:
			t.Fatalf("read pong: %v (handler error: %v)", err, modErr)
		default:
			t.Fatalf("read pong: %v", err)
		}
	}
	if reply.Type != 0x41 || string(reply.Payload) != "pong" {
		t.Fatalf("got frame %d %q, want 0x41 pong", reply.Type, reply.Payload)
	}
	_ = stream.Close()

	select {
	case p := <-mod.got:
		if p.Device != a.id.DeviceID {
			t.Fatalf("handler sees device %x, want %x", p.Device, a.id.DeviceID)
		}
		if p.Node != a.id.NodeID {
			t.Fatalf("handler sees node %x, want %x", p.Node, a.id.NodeID)
		}
		if p.SID != peerOnA.SID {
			t.Fatalf("handler sid %x, want %x", p.SID, peerOnA.SID)
		}
		if p.Path != mux.PathDirect {
			t.Fatalf("handler path %v, want direct", p.Path)
		}
		if p.Local.Device == nil || p.Local.Device.ID() != [32]byte(b.id.DeviceID) {
			t.Fatal("handler's local identity is not the accepting device")
		}
		if p.Card == nil || p.Card.DeviceID != [32]byte(a.id.DeviceID) {
			t.Fatal("handler has no verified card of the initiator")
		}
	case err := <-mod.errs:
		t.Fatalf("fake module: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not receive the stream")
	}
}

// ---- D37

type captureEvents struct {
	mu   sync.Mutex
	list []node.Event
}

func (c *captureEvents) Emit(e node.Event) {
	c.mu.Lock()
	c.list = append(c.list, e)
	c.mu.Unlock()
}

func (c *captureEvents) has(kind node.EventKind) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.list {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// TestNoCoverDoesNotPublishPublicCardButServesPeers pins D37: a node without a camouflaged
// listener starts, publishes no public card, tells the operator, and still serves a peer that
// holds its peer card.
func TestNoCoverDoesNotPublishPublicCardButServesPeers(t *testing.T) {
	queue := newCarrierQueue()
	events := &captureEvents{}
	b := newTestNode(t, cards.Visible, false, []carrier.Listener{queue}, events)

	b.start(t)

	if _, ok := b.rt.Cards().Own(objects.ScopePublic); ok {
		t.Fatal("a node without a cover published a public card")
	}
	if !events.has(node.EventNoCover) {
		t.Fatal("the operator was not told that the node has no public card")
	}

	a := newTestNode(t, cards.Hidden, false, nil, nil)
	a.start(t)

	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	peer, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: pair.Client,
	})
	if err != nil {
		t.Fatalf("Session to a no-cover node: %v", err)
	}
	if peer.Device != b.id.DeviceID {
		t.Fatalf("session peer %x, want %x", peer.Device, b.id.DeviceID)
	}
	_ = sess.Close()
}
