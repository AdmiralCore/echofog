package node_test

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

type mailboxWorld struct {
	f, m, r, s *testNode
	qF, qM     *carrierQueue
	fDials     int
	mu         sync.Mutex
}

func mailboxRelay(t *testing.T, q *carrierQueue) *testNode {
	t.Helper()
	return newTestNodeWith(t, cards.Visible, true, []carrier.Listener{q}, nil, func(c *node.Config) {
		c.Profile = node.Profile{Platform: node.PlatformDesktop, Roles: []node.Role{node.RoleMailbox}}
	})
}

func newMailboxWorld(t *testing.T) *mailboxWorld {
	t.Helper()
	w := &mailboxWorld{qF: newCarrierQueue(), qM: newCarrierQueue()}
	w.f, w.m = mailboxRelay(t, w.qF), mailboxRelay(t, w.qM)
	w.r = newTestNode(t, cards.Hidden, false, nil, nil)
	w.s = newTestNode(t, cards.Hidden, false, nil, nil)
	for _, n := range []*testNode{w.f, w.m, w.r, w.s} {
		n.start(t)
	}
	// F reaches M over an in-memory carrier, and may dial the addresses of the test cards
	node.AllowRelayCardAddrs(func(netip.Addr) bool { return true })
	t.Cleanup(func() { node.AllowRelayCardAddrs(func(netip.Addr) bool { return true }) })
	node.SetMailboxDialHook(w.f.rt, func(card *objects.Card) node.DialOpt {
		pair := memcarrier.NewQUIC()
		w.qM.push(pair.Server)
		w.mu.Lock()
		w.fDials++
		w.mu.Unlock()
		return node.DialOpt{Card: card, Carrier: pair.Client}
	})
	return w
}

// dialOpts gives a client the cards and in-memory carriers of F and M.
func (w *mailboxWorld) dialOpts(t *testing.T) func([32]byte) node.DialOpt {
	return func(device [32]byte) node.DialOpt {
		var n *testNode
		var q *carrierQueue
		switch device {
		case [32]byte(w.f.id.DeviceID):
			n, q = w.f, w.qF
		case [32]byte(w.m.id.DeviceID):
			n, q = w.m, w.qM
		default:
			return node.DialOpt{}
		}
		pair := memcarrier.NewQUIC()
		q.push(pair.Server)
		return node.DialOpt{Card: n.publicCard(t), Carrier: pair.Client}
	}
}

func (w *mailboxWorld) target(t *testing.T, queue []byte) mailbox.Target {
	mc := w.m.publicCard(t)
	return mailbox.Target{Relay: [32]byte(w.m.id.DeviceID), HS: mc.HsKey.Key(), Queue: queue, RelayCard: mc.Env.Raw}
}

func newClientReceiver(t *testing.T, w *mailboxWorld, got *[][]byte, mu *sync.Mutex) *mailbox.Receiver {
	t.Helper()
	r, err := mailbox.NewReceiver(mailbox.ReceiverConfig{
		Store: w.r.store.Store("v2/mailbox/client"),
		Dial:  w.r.rt.MailboxDialer(w.dialOpts(t)),
		Relays: func() [][32]byte {
			return [][32]byte{[32]byte(w.m.id.DeviceID)}
		},
		Now: time.Now, Wait: time.Second,
		Handle: func(ctx context.Context, blob []byte) bool {
			mu.Lock()
			*got = append(*got, append([]byte(nil), blob...))
			mu.Unlock()
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The recipient creates a queue on M, the sender puts messages in it through F over real sessions, the
// recipient fetches and acknowledges. F forwards to M with the card the sender supplied.
func TestMailboxClientThroughRealSessions(t *testing.T) {
	w := newMailboxWorld(t)
	var (
		got [][]byte
		mu  sync.Mutex
	)
	r := newClientReceiver(t, w, &got, &mu)
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	q := r.Queues()
	if len(q) != 1 {
		t.Fatalf("%d queues", len(q))
	}
	s := &mailbox.Sender{Dial: w.s.rt.MailboxDialer(w.dialOpts(t))}
	for i := 0; i < 5; i++ {
		st, err := s.Deposit(context.Background(), [32]byte(w.f.id.DeviceID), w.target(t, q[0].Queue[:]), []byte(fmt.Sprint("message ", i)))
		if err != nil || st != mailbox.StatusStored {
			t.Fatalf("deposit %d: %d %v", i, st, err)
		}
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 5 {
		t.Fatalf("%d of 5 messages fetched", n)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n = len(got)
	mu.Unlock()
	if n != 5 {
		t.Fatalf("%d messages after the acknowledgement, want 5", n)
	}
	if w.fDials == 0 {
		t.Fatal("F did not dial M")
	}
}

// F never dials a private address from a relay card the sender supplied.
func TestForwardingRelayRefusesAPrivateRelayCard(t *testing.T) {
	w := newMailboxWorld(t)
	// the addresses of the test cards are in the documentation range 203.0.113.0/24: refuse that one
	doc := netip.MustParsePrefix("203.0.113.0/24")
	node.AllowRelayCardAddrs(func(ip netip.Addr) bool { return !doc.Contains(ip) && !ip.IsLoopback() && !ip.IsPrivate() })
	var (
		got [][]byte
		mu  sync.Mutex
	)
	r := newClientReceiver(t, w, &got, &mu)
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	q := r.Queues()[0]
	s := &mailbox.Sender{Dial: w.s.rt.MailboxDialer(w.dialOpts(t))}
	st, err := s.Deposit(context.Background(), [32]byte(w.f.id.DeviceID), w.target(t, q.Queue[:]), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if st == mailbox.StatusStored {
		t.Fatal("F dialed a loopback address from a card")
	}
	if w.fDials != 0 {
		t.Fatalf("F dialed %d times", w.fDials)
	}
}

// A card that does not belong to the relay named in FORWARD is not used either.
func TestForwardingRelayRefusesACardOfAnotherDevice(t *testing.T) {
	w := newMailboxWorld(t)
	var (
		got [][]byte
		mu  sync.Mutex
	)
	r := newClientReceiver(t, w, &got, &mu)
	_ = r.Tick(context.Background())
	q := r.Queues()[0]
	tg := w.target(t, q.Queue[:])
	tg.RelayCard = w.f.publicCard(t).Env.Raw // a valid card, but not M's
	s := &mailbox.Sender{Dial: w.s.rt.MailboxDialer(w.dialOpts(t))}
	st, err := s.Deposit(context.Background(), [32]byte(w.f.id.DeviceID), tg, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if st == mailbox.StatusStored || w.fDials != 0 {
		t.Fatalf("status %d, %d dials", st, w.fDials)
	}
}

// A node without the mailbox role offers the service as a client only: it cannot be asked to store.
func TestClientOnlyNodeDoesNotServeMailbox(t *testing.T) {
	w := newMailboxWorld(t)
	opts := func(device [32]byte) node.DialOpt { return node.DialOpt{} }
	_ = opts
	// the recipient node (no role) is asked to create a queue by the sender
	q := newCarrierQueue()
	hub := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{q}, nil, nil)
	hub.start(t)
	pair := memcarrier.NewQUIC()
	q.push(pair.Server)
	d := w.s.rt.MailboxDialer(func([32]byte) node.DialOpt {
		return node.DialOpt{Card: hub.publicCard(t), Carrier: pair.Client}
	})
	conn, err := d.Dial(context.Background(), [32]byte(hub.id.DeviceID))
	if err == nil {
		_, cerr := conn.CreateQueue(context.Background(), mailbox.QueueCreate{Req: 1, Queue: bytes.Repeat([]byte{1}, 16), AuthKey: bytes.Repeat([]byte{2}, 32)})
		_ = conn.Close()
		if cerr == nil {
			t.Fatal("a node without the mailbox role created a queue")
		}
	}
}

// A chat node with MailboxRelays keeps a queue on M: the queue exists, and a deposit of a sender
// reaches the node's chat service through the fetch loop (the item is acknowledged once handled).
func TestChatNodeKeepsAQueueOnItsMailboxRelay(t *testing.T) {
	w := newMailboxWorld(t)
	var contacts [][32]byte
	rcpt := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) {
		c.Chat = true
		c.MailboxRelays = func() [][32]byte { return [][32]byte{[32]byte(w.m.id.DeviceID)} }
		c.ChatContacts = func() [][32]byte { return contacts }
	})
	rcpt.start(t)
	node.SetMailboxClientOpts(rcpt.rt, w.dialOpts(t))
	if err := node.MailboxReceiverTick(rcpt.rt, context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	q := node.MailboxQueues(rcpt.rt)
	if len(q) != 1 || q[0].Relay != [32]byte(w.m.id.DeviceID) {
		t.Fatalf("queues %v", q)
	}
	// a deposit of garbage reaches the chat service, which finds it final and lets it go
	s := &mailbox.Sender{Dial: w.s.rt.MailboxDialer(w.dialOpts(t))}
	st, err := s.Deposit(context.Background(), [32]byte(w.f.id.DeviceID), w.target(t, q[0].Queue[:]), bytes.Repeat([]byte{1}, 200))
	if err != nil || st != mailbox.StatusStored {
		t.Fatalf("deposit: %d %v", st, err)
	}
	if err := node.MailboxReceiverTick(rcpt.rt, context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := node.MailboxItems(w.m.rt); n != 0 {
		t.Fatalf("M still holds %d items: the chat service did not take them", n)
	}
}
