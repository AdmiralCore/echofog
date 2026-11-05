package mailbox_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// ---- two real runtimes on an in-memory carrier ------------------------------------------------

type testNode struct {
	account *identity.Account
	device  *identity.Device
	id      node.Identity
	rt      *node.Runtime
	store   *store.Store
}

func newNode(t *testing.T, profile node.Profile, listeners []carrier.Listener) *testNode {
	t.Helper()
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	devices, err := tw.DeviceList(1, time.Now())
	if err != nil {
		tw.Destroy()
		t.Fatalf("DeviceList: %v", err)
	}
	builder, err := cards.New(cards.Config{
		Account:    tw.Account.Public(),
		Device:     tw.Device,
		HsKey:      tw.Device.Handshake().Public(),
		L1Suites:   []uint32{uint32(objects.SuiteNoise3)},
		Role:       objects.RoleUser,
		Listeners:  []cards.Listener{{Suite: objects.SuiteNoise3, Host: "127.0.0.1", Port: 4430, Mask: tw.Device.Masking().Public()}},
		Devices:    devices,
		Visibility: cards.Visible,
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
	rt, err := node.New(node.Config{
		Account:   tw.Account,
		Device:    tw.Device,
		Cards:     builder,
		Store:     st,
		Listeners: listeners,
		Profile:   profile,
	})
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
		t.Fatalf("Start: %v", err)
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
		t.Fatalf("verify peer card: %v", err)
	}
	return card
}

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

// ---- helpers

func send(t *testing.T, st *mux.Stream, typ uint64, body []byte) {
	t.Helper()
	if err := st.WriteFrame(typ, 0, body); err != nil {
		t.Fatalf("write frame 0x%x: %v", typ, err)
	}
}

func recv(t *testing.T, st *mux.Stream) (uint64, []byte) {
	t.Helper()
	if err := st.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	f, err := st.ReadFrame()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f.Type, f.Payload
}

func openMailbox(t *testing.T, from, to *testNode, q *carrierQueue) (node.Peer, *mux.Stream) {
	t.Helper()
	pair := memcarrier.NewQUIC()
	q.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	peer, st, err := from.rt.Open(ctx, to.id.DeviceID, service.Mailbox, node.DialOpt{
		Card:    to.peerCard(t),
		Carrier: pair.Client,
	})
	if err != nil {
		t.Fatalf("Open mailbox: %v", err)
	}
	return peer, st
}

func authSigner(t *testing.T) *keys.Signer {
	t.Helper()
	s, err := keys.NewEd25519Signer(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// registerClient makes a test node offer mailbox/2 as a client. T7.4 adds the real client; here a
// handler is required by the runtime API and the test node has no listener, so it never serves.
func registerClient(t *testing.T, n *testNode) {
	t.Helper()
	spec, ok := service.Builtin(service.Mailbox)
	if !ok {
		t.Fatal("mailbox is not a built-in service")
	}
	n.rt.Register(spec, func(_ context.Context, _ node.Peer, st *mux.Stream) { _ = st.Close() })
}

// ---- end-to-end

func TestMailboxServiceOverTwoRuntimes(t *testing.T) {
	q := newCarrierQueue()
	m := newNode(t, node.Profile{Platform: "server", Roles: []string{"mailbox"}}, []carrier.Listener{q})
	c := newNode(t, node.Profile{Platform: "test"}, nil)
	registerClient(t, c)
	m.start(t)
	c.start(t)

	peer, st := openMailbox(t, c, m, q)
	defer st.Close()

	queue := bytes.Repeat([]byte{0x90}, mailbox.QueueSize)
	auth := authSigner(t)

	send(t, st, mailbox.FrameQueueCreate, mailbox.QueueCreate{
		Req: 1, Queue: queue, AuthKey: auth.Public(), TTL: 3600, Public: false,
	}.Append(nil))
	typ, payload := recv(t, st)
	if typ != mailbox.FrameQueueOK {
		t.Fatalf("queue create answer 0x%x", typ)
	}
	ok, err := mailbox.ParseQueueOK(payload)
	if err != nil || ok.Req != 1 || ok.Expires == 0 {
		t.Fatalf("QUEUE_OK %+v %v", ok, err)
	}

	send(t, st, mailbox.FrameDeposit, mailbox.Deposit{Req: 2, Queue: queue, Blob: []byte("offline message")}.Append(nil))
	typ, payload = recv(t, st)
	if typ != mailbox.FrameDeposited {
		t.Fatalf("deposit answer 0x%x", typ)
	}
	dep, err := mailbox.ParseDeposited(payload)
	if err != nil || dep.Req != 2 || dep.Status != mailbox.StatusStored || len(dep.Item) != mailbox.ItemSize {
		t.Fatalf("DEPOSITED %+v %v", dep, err)
	}

	send(t, st, mailbox.FrameFetch, mailbox.Fetch{
		Req: 3, Queue: queue, Max: 10, Wait: 1,
		Auth: auth.Sign(mailbox.AuthInput(mailbox.OpFetch, peer.SID, queue, 3)),
	}.Append(nil))
	typ, payload = recv(t, st)
	if typ != mailbox.FrameItems {
		t.Fatalf("fetch answer 0x%x", typ)
	}
	items, err := mailbox.ParseItems(payload)
	if err != nil || items.Req != 3 || len(items.Items) != 1 || string(items.Items[0].Blob) != "offline message" {
		t.Fatalf("ITEMS %+v %v", items, err)
	}

	send(t, st, mailbox.FrameAck, mailbox.Ack{
		Req: 4, Queue: queue, IDs: [][]byte{items.Items[0].ID},
		Auth: auth.Sign(mailbox.AuthInput(mailbox.OpAck, peer.SID, queue, 4)),
	}.Append(nil))

	// ACK has no answer frame; a following FETCH shows it took effect.
	send(t, st, mailbox.FrameFetch, mailbox.Fetch{
		Req: 5, Queue: queue, Max: 10,
		Auth: auth.Sign(mailbox.AuthInput(mailbox.OpFetch, peer.SID, queue, 5)),
	}.Append(nil))
	typ, payload = recv(t, st)
	if typ != mailbox.FrameItems {
		t.Fatalf("second fetch answer 0x%x", typ)
	}
	items, err = mailbox.ParseItems(payload)
	if err != nil || len(items.Items) != 0 {
		t.Fatalf("ACK left %d items (%v)", len(items.Items), err)
	}
}

func TestMailboxWithoutTheRoleIsNotServed(t *testing.T) {
	q := newCarrierQueue()
	m := newNode(t, node.Profile{Platform: "test"}, []carrier.Listener{q})
	c := newNode(t, node.Profile{Platform: "test"}, nil)
	registerClient(t, c)
	m.start(t)
	c.start(t)

	pair := memcarrier.NewQUIC()
	q.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, st, err := c.rt.Open(ctx, m.id.DeviceID, service.Mailbox, node.DialOpt{
		Card: m.peerCard(t), Carrier: pair.Client,
	}); err == nil {
		_ = st.Close()
		t.Fatal("a node without the mailbox role offered mailbox/2")
	}
}

func TestLongPollDoesNotBlockTheStream(t *testing.T) {
	q := newCarrierQueue()
	m := newNode(t, node.Profile{Platform: "server", Roles: []string{"mailbox"}}, []carrier.Listener{q})
	c := newNode(t, node.Profile{Platform: "test"}, nil)
	registerClient(t, c)
	m.start(t)
	c.start(t)

	peer, st := openMailbox(t, c, m, q)
	defer st.Close()

	queue := bytes.Repeat([]byte{0x91}, mailbox.QueueSize)
	auth := authSigner(t)
	send(t, st, mailbox.FrameQueueCreate, mailbox.QueueCreate{
		Req: 1, Queue: queue, AuthKey: auth.Public(),
	}.Append(nil))
	if typ, _ := recv(t, st); typ != mailbox.FrameQueueOK {
		t.Fatalf("queue create answer 0x%x", typ)
	}

	// A FETCH with a long wait on an empty queue and a DEPOSIT on the same stream: the server
	// must read both and answer out of order (services.md A15).
	send(t, st, mailbox.FrameFetch, mailbox.Fetch{
		Req: 2, Queue: queue, Max: 10, Wait: 30,
		Auth: auth.Sign(mailbox.AuthInput(mailbox.OpFetch, peer.SID, queue, 2)),
	}.Append(nil))
	send(t, st, mailbox.FrameDeposit, mailbox.Deposit{Req: 3, Queue: queue, Blob: []byte("later")}.Append(nil))

	got := map[uint64]bool{}
	for len(got) < 2 {
		typ, payload := recv(t, st)
		switch typ {
		case mailbox.FrameItems:
			m, err := mailbox.ParseItems(payload)
			if err != nil || len(m.Items) != 1 || string(m.Items[0].Blob) != "later" {
				t.Fatalf("ITEMS %+v %v", m, err)
			}
			got[typ] = true
		case mailbox.FrameDeposited:
			m, err := mailbox.ParseDeposited(payload)
			if err != nil || m.Status != mailbox.StatusStored {
				t.Fatalf("DEPOSITED %+v %v", m, err)
			}
			got[typ] = true
		default:
			t.Fatalf("unexpected frame 0x%x", typ)
		}
	}
}

func TestFetchAuthFromAnotherStreamIsRefused(t *testing.T) {
	q := newCarrierQueue()
	m := newNode(t, node.Profile{Platform: "server", Roles: []string{"mailbox"}}, []carrier.Listener{q})
	c := newNode(t, node.Profile{Platform: "test"}, nil)
	registerClient(t, c)
	m.start(t)
	c.start(t)

	peer, st := openMailbox(t, c, m, q)
	defer st.Close()
	queue := bytes.Repeat([]byte{0x92}, mailbox.QueueSize)
	auth := authSigner(t)
	send(t, st, mailbox.FrameQueueCreate, mailbox.QueueCreate{Req: 1, Queue: queue, AuthKey: auth.Public()}.Append(nil))
	if typ, _ := recv(t, st); typ != mailbox.FrameQueueOK {
		t.Fatalf("queue create answer 0x%x", typ)
	}

	var wrongSID [16]byte
	copy(wrongSID[:], "another-session!")
	send(t, st, mailbox.FrameFetch, mailbox.Fetch{
		Req: 2, Queue: queue, Max: 10,
		Auth: auth.Sign(mailbox.AuthInput(mailbox.OpFetch, wrongSID, queue, 2)),
	}.Append(nil))
	if err := st.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := st.ReadFrame()
	var ce *mux.CloseError
	if !errors.As(err, &ce) || ce.Code != frame.CodeRefused {
		t.Fatalf("auth failure: %v, want CLOSE{refused}", err)
	}
	_ = peer
}
