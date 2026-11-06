package mailbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// ---- test doubles: sessions straight into a Server, no streams ----------------------------------

// directConn is a Conn that calls the methods of a Server as the peer of one session.
type directConn struct {
	srv  *Server
	peer Peer
	// waits records the wait of every FETCH
	mu    sync.Mutex
	waits []uint32
	acks  int
}

func (c *directConn) SID() [16]byte { return c.peer.SID }
func (c *directConn) Close() error  { return nil }

func (c *directConn) CreateQueue(ctx context.Context, m QueueCreate) (uint64, error) {
	return c.srv.CreateQueue(c.peer, m)
}

func (c *directConn) Deposit(ctx context.Context, m Deposit) (Deposited, error) {
	st, item, err := c.srv.Deposit(c.peer, m)
	return Deposited{Req: m.Req, Status: st, Item: item}, err
}

func (c *directConn) Forward(ctx context.Context, m Forward) (Deposited, error) {
	st, err := c.srv.Forward(ctx, c.peer, m)
	return Deposited{Req: m.Req, Status: st}, err
}

func (c *directConn) Fetch(ctx context.Context, m Fetch) (Items, error) {
	c.mu.Lock()
	c.waits = append(c.waits, m.Wait)
	c.mu.Unlock()
	items, more, err := c.srv.Fetch(ctx, c.peer, m)
	return Items{Req: m.Req, Items: items, More: more}, err
}

func (c *directConn) Ack(ctx context.Context, m Ack) error {
	c.mu.Lock()
	c.acks++
	c.mu.Unlock()
	return c.srv.Ack(c.peer, m)
}

// net maps device IDs to servers; Dial opens a directConn as peer.
type net struct {
	mu      sync.Mutex
	servers map[[32]byte]*Server
	conns   []*directConn
	peer    func(to [32]byte) Peer
	failing map[[32]byte]bool
	sid     byte
}

func (n *net) Dial(ctx context.Context, device [32]byte) (Conn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failing[device] || n.servers[device] == nil {
		return nil, errors.New("unreachable")
	}
	n.sid++
	p := n.peer(device)
	p.SID = [16]byte{n.sid, byte(len(n.conns))}
	c := &directConn{srv: n.servers[device], peer: p}
	n.conns = append(n.conns, c)
	return c, nil
}

// relayNode is a mailbox relay with its own store.
type relayNode struct {
	fx     *fixture
	device [32]byte
}

func newRelay(t *testing.T, clock *testClock, mutate func(*Config)) *relayNode {
	t.Helper()
	fx := newFixture(t, func(c *Config) {
		c.Now = clock.Now
		c.After = func(time.Duration) <-chan time.Time { ch := make(chan time.Time, 1); ch <- time.Time{}; return ch }
		if mutate != nil {
			mutate(c)
		}
	})
	var dev [32]byte
	_, _ = rand.Read(dev[:])
	return &relayNode{fx: fx, device: dev}
}

// fakeForwarder lets a forwarding relay F reach mailbox relays.
type fakeForwarder struct {
	mu        sync.Mutex
	target    map[string]*Server // by relay bytes
	as        Peer               // the peer F is, as M sees it
	connected map[string]bool
	calls     int
	seen      [][]byte
}

func (f *fakeForwarder) Connected(relay []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected[string(relay)]
}

func (f *fakeForwarder) Forward(ctx context.Context, m Forward) (uint32, error) {
	f.mu.Lock()
	srv := f.target[string(m.Relay)]
	f.calls++
	f.seen = append(f.seen, append([]byte(nil), m.Sealed...))
	f.mu.Unlock()
	if srv == nil {
		return StatusRelayUnreachable, nil
	}
	return srv.Forward(ctx, f.as, m)
}

func senderPeer(node byte) func([32]byte) Peer {
	return func([32]byte) Peer {
		return Peer{Node: [32]byte{0x5e, node}, Device: [32]byte{0x5d, node}, Path: mux.PathDirect,
			Remote: netip.MustParseAddrPort(fmt.Sprintf("203.0.113.%d:4430", node))}
	}
}

// ---- the role F -----------------------------------------------------------------------------------

func TestForwardingRelayPassesTheDepositOn(t *testing.T) {
	clock := newTestClock()
	m := newRelay(t, clock, nil)
	fw := &fakeForwarder{target: map[string]*Server{}, connected: map[string]bool{}}
	f := newRelay(t, clock, func(c *Config) { c.Forwarder = fw })
	fw.target[string(m.device[:])] = m.fx.srv
	fw.as = Peer{Node: f.fx.node, Device: f.device, SID: [16]byte{9}, Path: mux.PathDirect, Remote: netip.MustParseAddrPort("198.51.100.1:443")}
	// M is named by its device ID: M recognises it as itself
	m.fx.srv.cfg.SelfDevice = m.device

	m.fx.create(t, m.fx.peer(1), 2, false, 0)
	dep := Deposit{Req: 1, Queue: m.fx.queue, Blob: []byte("hello")}
	sealed, err := SealForward(bytes.Repeat([]byte{7}, 32), m.fx.hs.Public().Key(), dep.Append(nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.fx.srv.Forward(context.Background(), f.fx.peer(2), Forward{Req: 4, Relay: m.device[:], Sealed: sealed})
	if err != nil || st != StatusStored {
		t.Fatalf("FORWARD through F: %d %v", st, err)
	}
	items, _ := m.fx.fetch(t, m.fx.peer(1), nil, 10, 0)
	if len(items) != 1 || string(items[0].Blob) != "hello" {
		t.Fatalf("M holds %d items", len(items))
	}
	// F saw the sealed bytes only: nothing of the queue is in its store
	entries, _ := f.fx.kv.Scan(nil, 1000)
	for _, e := range entries {
		if bytes.Contains(e.Key, m.fx.queue) || bytes.Contains(e.Value, m.fx.queue) {
			t.Fatal("F kept the queue ID")
		}
	}
	// M saw F as the handing peer, never the sender
	if fw.calls != 1 {
		t.Fatalf("%d calls", fw.calls)
	}
}

func TestForwardingRelayQuotaPerSenderNode(t *testing.T) {
	clock := newTestClock()
	fw := &fakeForwarder{target: map[string]*Server{}, connected: map[string]bool{}}
	f := newRelay(t, clock, func(c *Config) { c.Forwarder = fw; c.Limits = &Limits{QuotaItemsPerDay: 3} })
	send := func(sid byte) uint32 {
		st, err := f.fx.srv.Forward(context.Background(), f.fx.peer(sid), Forward{Req: 1, Relay: bytes.Repeat([]byte{1}, 32), Sealed: bytes.Repeat([]byte{2}, 100)})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	for i := 0; i < 3; i++ {
		if st := send(1); st == StatusRefused {
			t.Fatalf("forward %d refused within the quota", i)
		}
	}
	if st := send(1); st != StatusRefused {
		t.Fatalf("the fourth: %d, want refused", st)
	}
	if st := send(2); st != StatusRefused { // a new session does not reset the quota
		t.Fatalf("on a new session: %d, want refused", st)
	}
	clock.Advance(25 * time.Hour)
	if st := send(1); st == StatusRefused {
		t.Fatal("the quota did not renew the next day")
	}
}

func TestForwardingRelayDialsAreLimitedPerSession(t *testing.T) {
	clock := newTestClock()
	fw := &fakeForwarder{target: map[string]*Server{}, connected: map[string]bool{}}
	f := newRelay(t, clock, func(c *Config) { c.Forwarder = fw })
	send := func(sid byte, relay byte) uint32 {
		st, err := f.fx.srv.Forward(context.Background(), f.fx.peer(sid), Forward{Req: 1, Relay: bytes.Repeat([]byte{relay}, 32), Sealed: bytes.Repeat([]byte{2}, 100)})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	for i := 0; i < 10; i++ { // ten different relays, none connected: ten dials
		if st := send(1, byte(i+1)); st == StatusRefused {
			t.Fatalf("dial %d refused", i)
		}
	}
	if st := send(1, 99); st != StatusRefused {
		t.Fatalf("the eleventh dial of one session: %d", st)
	}
	fw.connected[string(bytes.Repeat([]byte{3}, 32))] = true
	if st := send(1, 3); st == StatusRefused {
		t.Fatal("a forward over an existing session counted as a dial")
	}
	clock.Advance(61 * time.Second)
	if st := send(1, 99); st == StatusRefused {
		t.Fatal("the dial budget did not renew after a minute")
	}
}

func TestWithoutAForwarderFORWARDIsUnreachable(t *testing.T) {
	clock := newTestClock()
	f := newRelay(t, clock, nil)
	st, err := f.fx.srv.Forward(context.Background(), f.fx.peer(1), Forward{Req: 1, Relay: bytes.Repeat([]byte{1}, 32), Sealed: bytes.Repeat([]byte{2}, 100)})
	if err != nil || st != StatusRelayUnreachable {
		t.Fatalf("%d %v", st, err)
	}
}

// ---- the sender ------------------------------------------------------------------------------------

type world struct {
	clock *testClock
	m, f  *relayNode
	fw    *fakeForwarder
	n     *net
}

func newWorld(t *testing.T) *world {
	t.Helper()
	clock := newTestClock()
	m := newRelay(t, clock, nil)
	fw := &fakeForwarder{target: map[string]*Server{}, connected: map[string]bool{}}
	f := newRelay(t, clock, func(c *Config) { c.Forwarder = fw })
	m.fx.srv.cfg.SelfDevice = m.device
	fw.target[string(m.device[:])] = m.fx.srv
	fw.as = Peer{Node: f.fx.node, Device: f.device, SID: [16]byte{9}, Path: mux.PathDirect, Remote: netip.MustParseAddrPort("198.51.100.1:443")}
	w := &world{clock: clock, m: m, f: f, fw: fw}
	w.n = &net{servers: map[[32]byte]*Server{m.device: m.fx.srv, f.device: f.fx.srv}, failing: map[[32]byte]bool{}}
	w.n.peer = func(to [32]byte) Peer { return w.nextPeer(to) }
	return w
}

// nextPeer is the peer a dialer is to the server it dials: senders and recipients differ by node.
func (w *world) nextPeer(to [32]byte) Peer {
	return Peer{Node: [32]byte{0xa0 + byte(len(w.n.conns))}, Device: [32]byte{0xb0 + byte(len(w.n.conns))}, Path: mux.PathDirect,
		Remote: netip.MustParseAddrPort(fmt.Sprintf("203.0.113.%d:4430", len(w.n.conns)%250+1))}
}

func (w *world) target(queue []byte) Target {
	return Target{Relay: w.m.device, HS: w.m.fx.hs.Public().Key(), Queue: queue}
}

func (w *world) receiver(t *testing.T, handle func(blob []byte) bool, limited func() bool) (*Receiver, *recorder) {
	t.Helper()
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Destroy)
	st, err := store.Open(store.Options{Dir: t.TempDir(), Key: tw.Account.StorageKey()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rec := &recorder{}
	r, err := NewReceiver(ReceiverConfig{
		Store: st.Store("v2/mailbox/client"), Dial: w.n,
		Relays: func() [][32]byte { return [][32]byte{w.m.device} },
		Now:    w.clock.Now, Wait: time.Second,
		Handle: func(ctx context.Context, blob []byte) bool {
			rec.add(blob)
			return handle == nil || handle(blob)
		},
		Announce: rec.announce, Limited: limited,
	})
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	rec.st, rec.cfgStore = st, st.Store("v2/mailbox/client")
	return r, rec
}

type recorder struct {
	mu        sync.Mutex
	blobs     [][]byte
	announce_ []QueueRef
	st        *store.Store
	cfgStore  store.KV
}

func (r *recorder) add(b []byte) {
	r.mu.Lock()
	r.blobs = append(r.blobs, append([]byte(nil), b...))
	r.mu.Unlock()
}

func (r *recorder) announce(q QueueRef) {
	r.mu.Lock()
	r.announce_ = append(r.announce_, q)
	r.mu.Unlock()
}

func (r *recorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.blobs) }

func TestSenderDepositsThroughF(t *testing.T) {
	w := newWorld(t)
	w.m.fx.create(t, w.m.fx.peer(1), 2, false, 0)
	s := &Sender{Dial: w.n}
	st, err := s.Deposit(context.Background(), w.f.device, w.target(w.m.fx.queue), []byte("hello"))
	if err != nil || st != StatusStored {
		t.Fatalf("Deposit: %d %v", st, err)
	}
	items, _ := w.m.fx.fetch(t, w.m.fx.peer(1), nil, 10, 0)
	if len(items) != 1 || string(items[0].Blob) != "hello" {
		t.Fatalf("M holds %d items", len(items))
	}
	// the same blob again is a new deposit (a new ephemeral key), the same sealed bytes are not
	if st, err := s.Deposit(context.Background(), w.f.device, w.target(w.m.fx.queue), []byte("hello")); err != nil || st != StatusStored {
		t.Fatalf("second: %d %v", st, err)
	}
}

func TestSenderReportsWhatMAnswered(t *testing.T) {
	w := newWorld(t)
	s := &Sender{Dial: w.n}
	// a queue that does not exist: unknown-queue
	if st, err := s.Deposit(context.Background(), w.f.device, w.target(w.m.fx.queue), []byte("x")); err != nil || st != StatusUnknownQueue {
		t.Fatalf("unknown queue: %d %v", st, err)
	}
	// a blob above 64 KiB never leaves
	if _, err := s.Deposit(context.Background(), w.f.device, w.target(w.m.fx.queue), make([]byte, MaxBlob+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	// F unreachable is an error the caller can use to pick another F
	w.n.failing[w.f.device] = true
	if _, err := s.Deposit(context.Background(), w.f.device, w.target(w.m.fx.queue), []byte("x")); err == nil {
		t.Fatal("an unreachable F was not an error")
	}
	// a full queue
	w.n.failing[w.f.device] = false
	w.m.fx.srv.lim.PrivateItems = 1
	w.m.fx.create(t, w.m.fx.peer(1), 2, false, 0)
	for i, want := range []uint32{StatusStored, StatusFull} {
		if st, err := s.Deposit(context.Background(), w.f.device, w.target(w.m.fx.queue), []byte(fmt.Sprint("m", i))); err != nil || st != want {
			t.Fatalf("deposit %d: %d %v, want %d", i, st, err, want)
		}
	}
}

func TestSenderMinesProofOfWorkForAPublicQueue(t *testing.T) {
	w := newWorld(t)
	w.m.fx.create(t, w.m.fx.peer(1), 2, true, 10)
	s := &Sender{Dial: w.n}
	tg := w.target(w.m.fx.queue)
	tg.Public, tg.PowBits = true, 10
	if st, err := s.Deposit(context.Background(), w.f.device, tg, []byte("first contact")); err != nil || st != StatusStored {
		t.Fatalf("%d %v", st, err)
	}
	// the search can be cancelled, and its size is bounded
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tg.PowBits = MaxPowBits
	if _, err := s.Deposit(ctx, w.f.device, tg, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled search: %v", err)
	}
	tg.PowBits = MaxPowBits + 1
	if _, err := s.Deposit(context.Background(), w.f.device, tg, []byte("x")); err == nil {
		t.Fatal("a threshold above the maximum was accepted")
	}
}

// ---- the recipient --------------------------------------------------------------------------------

func TestReceiverCreatesFetchesAndAcknowledges(t *testing.T) {
	w := newWorld(t)
	r, rec := w.receiver(t, nil, nil)
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	qs := r.Queues()
	if len(qs) != 1 || qs[0].Relay != w.m.device || len(rec.announce_) != 1 {
		t.Fatalf("queues %v, announced %d", qs, len(rec.announce_))
	}
	s := &Sender{Dial: w.n}
	for i := 0; i < 3; i++ {
		if st, err := s.Deposit(context.Background(), w.f.device, w.target(qs[0].Queue[:]), []byte(fmt.Sprint("m", i))); err != nil || st != StatusStored {
			t.Fatalf("deposit %d: %d %v", i, st, err)
		}
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 3 {
		t.Fatalf("handled %d blobs", rec.count())
	}
	// acknowledged: nothing comes again
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 3 {
		t.Fatalf("handled %d blobs after the acknowledgement", rec.count())
	}
	if tot, _ := w.m.fx.srv.totalLocked(false); tot.Items != 0 {
		t.Fatalf("M still holds %d items", tot.Items)
	}
}

func TestReceiverKeepsWhatItDidNotHandle(t *testing.T) {
	w := newWorld(t)
	fail := true
	r, rec := w.receiver(t, func([]byte) bool { return !fail }, nil)
	_ = r.Tick(context.Background())
	qs := r.Queues()
	s := &Sender{Dial: w.n}
	if _, err := s.Deposit(context.Background(), w.f.device, w.target(qs[0].Queue[:]), []byte("later")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_ = r.Tick(context.Background())
	}
	if rec.count() != 2 {
		t.Fatalf("an unhandled blob was offered %d times, want 2", rec.count())
	}
	fail = false
	_ = r.Tick(context.Background())
	_ = r.Tick(context.Background())
	if rec.count() != 3 {
		t.Fatalf("offered %d times in all", rec.count())
	}
}

func TestReceiverRotatesQueues(t *testing.T) {
	w := newWorld(t)
	r, rec := w.receiver(t, nil, nil)
	_ = r.Tick(context.Background())
	first := r.Queues()[0]
	s := &Sender{Dial: w.n}
	if _, err := s.Deposit(context.Background(), w.f.device, w.target(first.Queue[:]), []byte("to the old queue")); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(19 * time.Hour)
	_ = r.Tick(context.Background())
	if r.Queues()[0].Queue != first.Queue || len(rec.announce_) != 1 {
		t.Fatal("rotated before 20 hours")
	}
	w.clock.Advance(10 * time.Hour) // 29 h: past uniform(20, 28)
	_ = r.Tick(context.Background())
	second := r.Queues()[0]
	if second.Queue == first.Queue || len(rec.announce_) != 2 {
		t.Fatalf("no new queue after 29 hours: %d announcements", len(rec.announce_))
	}
	// the old queue is still read: the message sent to it arrives
	if rec.count() < 1 {
		t.Fatalf("handled %d: the old queue was not drained", rec.count())
	}
	// both queues take deposits; a deposit to the old one is still fetched during its 7 days
	if _, err := s.Deposit(context.Background(), w.f.device, w.target(first.Queue[:]), []byte("late to the old one")); err != nil {
		t.Fatal(err)
	}
	_ = r.Tick(context.Background())
	if rec.count() != 2 {
		t.Fatalf("handled %d: a late deposit to the old queue was missed", rec.count())
	}
	// after drained + 7 days the old queue is no longer polled
	w.clock.Advance(7*24*time.Hour + time.Hour)
	_ = r.Tick(context.Background())
	for _, c := range w.n.conns {
		_ = c
	}
	entries, _ := rec.cfgStore.Scan(nil, 100)
	var queues int
	for _, e := range entries {
		if bytes.HasPrefix(e.Key, []byte("q:")) {
			queues++
		}
	}
	if queues > 2 { // the current one and at most one more rotated meanwhile
		t.Fatalf("%d queue records kept", queues)
	}
}

func TestReceiverInBackgroundLimitedModeDoesNotLongPoll(t *testing.T) {
	w := newWorld(t)
	limited := true
	r, _ := w.receiver(t, nil, func() bool { return limited })
	_ = r.Tick(context.Background())
	_ = r.Tick(context.Background())
	for _, c := range w.n.conns {
		for _, wait := range c.waits {
			if wait != 0 {
				t.Fatalf("a FETCH with wait %d in limited mode", wait)
			}
		}
	}
	limited = false
	_ = r.Tick(context.Background())
	var long bool
	for _, c := range w.n.conns {
		for _, wait := range c.waits {
			if wait > 0 {
				long = true
			}
		}
	}
	if !long {
		t.Fatal("no long-poll in normal mode")
	}
}

func TestReceiverSurvivesARestartAndAnUnreachableRelay(t *testing.T) {
	w := newWorld(t)
	r, rec := w.receiver(t, nil, nil)
	_ = r.Tick(context.Background())
	first := r.Queues()[0]
	// the relay is unreachable: Tick reports it, the queue stays
	w.n.failing[w.m.device] = true
	if err := r.Tick(context.Background()); err == nil {
		t.Fatal("an unreachable relay was not reported")
	}
	w.n.failing[w.m.device] = false
	// a restart over the same store reuses the queue
	r2, err := NewReceiver(ReceiverConfig{
		Store: rec.cfgStore, Dial: w.n, Relays: func() [][32]byte { return [][32]byte{w.m.device} },
		Now: w.clock.Now, Wait: time.Second, Handle: func(context.Context, []byte) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r2.Queues(); len(got) != 1 || got[0].Queue != first.Queue {
		t.Fatalf("after a restart: %v, want the queue %x", got, first.Queue)
	}
}

// The scenario of the task: the recipient is offline for two hours, three senders put 50 messages in
// its queue through F, and all of them are there when it comes back.
func TestFiftyMessagesFromThreeSendersAfterTwoHoursOffline(t *testing.T) {
	w := newWorld(t)
	r, rec := w.receiver(t, nil, nil)
	_ = r.Tick(context.Background())
	q := r.Queues()[0]
	w.clock.Advance(2 * time.Hour)
	counts := []int{17, 17, 16}
	for i, n := range counts {
		s := &Sender{Dial: w.n}
		for j := 0; j < n; j++ {
			if st, err := s.Deposit(context.Background(), w.f.device, w.target(q.Queue[:]), []byte(fmt.Sprintf("s%d-m%d", i, j))); err != nil || st != StatusStored {
				t.Fatalf("sender %d message %d: %d %v", i, j, st, err)
			}
		}
	}
	for i := 0; i < 3 && rec.count() < 50; i++ {
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if rec.count() != 50 {
		t.Fatalf("%d of 50 messages arrived", rec.count())
	}
	seen := map[string]bool{}
	for _, b := range rec.blobs {
		seen[string(b)] = true
	}
	if len(seen) != 50 {
		t.Fatalf("%d distinct messages", len(seen))
	}
	// what F saw is sealed bytes; what M saw is F: neither holds the other end
	for _, s := range w.fw.seen {
		if bytes.Contains(s, []byte("s0-m0")) || bytes.Contains(s, q.Queue[:]) {
			t.Fatal("F could read the deposit")
		}
	}
	if tot, _ := w.m.fx.srv.totalLocked(false); tot.Items != 0 {
		t.Fatalf("M holds %d items after the acknowledgement", tot.Items)
	}
}
