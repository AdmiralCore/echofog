package files

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/session"
)

var (
	fileServices = []handshake.ServiceOffer{{ID: 7, Versions: []uint32{2}}}
	fileSuites   = []uint16{handshake.SuiteXK, handshake.SuiteXX}
)

// testClock is the injected clock (AD13).
type testClock struct {
	mu     sync.Mutex
	now    time.Time
	seq    uint64
	timers map[uint64]*testTimer
}

type testTimer struct {
	c    *testClock
	id   uint64
	at   time.Time
	f    func()
	dead bool
}

func newTestClock() *testClock {
	return &testClock{now: time.Unix(1_800_000_000, 0), timers: map[uint64]*testTimer{}}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &testTimer{c: c, id: c.seq, at: c.now.Add(d), f: f}
	c.timers[t.id] = t
	return t
}

func (t *testTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.dead {
		return false
	}
	t.dead = true
	delete(t.c.timers, t.id)
	return true
}

// Advance moves the clock and fires the timers that became due.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*testTimer
	for _, t := range c.timers {
		if !t.dead && !t.at.After(c.now) {
			due = append(due, t)
		}
	}
	for _, t := range due {
		t.dead = true
		delete(c.timers, t.id)
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

// tnode is one identity. Identities are made once per test binary: memguard limits how many a
// Windows test process can hold.
type tnode struct {
	account *identity.Account
	device  *identity.Device
	devices []byte
	now     time.Time
	tw      *identity.Throwaway
}

func newTNode() *tnode {
	tw, err := identity.NewThrowaway()
	if err != nil {
		panic(err)
	}
	devices, err := tw.DeviceList(1, time.Now())
	if err != nil {
		panic(err)
	}
	return &tnode{account: tw.Account, device: tw.Device, devices: devices, now: time.Now(), tw: tw}
}

func (n *tnode) node() [32]byte { return n.account.NodeID() }

var (
	nodeA, nodeB, nodeC *tnode
)

func TestMain(m *testing.M) {
	nodeA, nodeB, nodeC = newTNode(), newTNode(), newTNode()
	code := m.Run()
	for _, n := range []*tnode{nodeA, nodeB, nodeC} {
		n.tw.Destroy()
	}
	os.Exit(code)
}

func (n *tnode) card(seq uint64) []byte {
	body := &pb.NodeCard{ProtoVer: handshake.ProtoVer, HsKey: n.device.Handshake().Public(),
		L1Suites: []uint32{uint32(handshake.SuiteXK), uint32(handshake.SuiteXX)},
		Account:  n.account.Public(), Devices: n.devices, Scope: uint32(objects.ScopePeer)}
	var env []byte
	err := n.device.WithSigner(func(s *keys.Signer) error {
		var err error
		env, err = objects.SealCard(s, seq, uint64(n.now.Add(24*time.Hour).Unix()), body)
		return err
	})
	if err != nil {
		panic(err)
	}
	return env
}

func (n *tnode) local(withOwn bool) session.LocalIdentity {
	peer := n.card(10)
	l := session.LocalIdentity{Device: n.device, Card: n.card(7), Suites: fileSuites,
		Features: []uint32{uint32(frame.FeaturePathMigration)}, Services: fileServices}
	if withOwn {
		l.OwnCards = handshake.OwnCards{
			Current: map[uint32]handshake.OwnCard{uint32(objects.ScopePeer): {Seq: 10, Envelope: peer}},
			Issued:  map[uint64]handshake.IssuedCard{10: {Scope: uint32(objects.ScopePeer), At: n.now}},
		}
	}
	return l
}

func (n *tnode) verified() *objects.Card {
	c, err := objects.VerifyCard(n.card(10), n.now)
	if err != nil {
		panic(err)
	}
	return c
}

type oneListener struct {
	c    mux.Carrier
	mu   sync.Mutex
	used bool
}

func (l *oneListener) Accept(ctx context.Context) (mux.Carrier, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	l.used = true
	return l.c, nil
}
func (l *oneListener) Addr() net.Addr { return nil }
func (l *oneListener) Close() error   { return nil }

// lab is node A (the holder, with a Server) and sessions from B and C to it.
type lab struct {
	t   *testing.T
	ctx context.Context
	srv *Server
	clk *testClock
}

func newLab(t *testing.T, cfg ServerConfig) *lab {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	clk := newTestClock()
	if cfg.Clock == nil {
		cfg.Clock = clk
	}
	return &lab{t: t, ctx: ctx, srv: NewServer(cfg), clk: clk}
}

// link is a session from a client node to the holder.
type link struct {
	client *mux.Session
	server *mux.Session
	pair   *memcarrier.Pair
}

func (l *lab) dial(from *tnode, opts ...memcarrier.Option) *link {
	t := l.t
	t.Helper()
	pair := memcarrier.NewBytes(opts...)
	dialed := make(chan *session.Established, 1)
	go func() {
		est, err := session.Dial(l.ctx, nodeA.verified(), carrier.Attempt{Kind: carrier.AttemptCarrier},
			from.local(false), session.WithCarrier(pair.Client))
		if err != nil {
			t.Errorf("dial: %v", err)
		}
		dialed <- est
	}()
	serverEst, err := session.Accept(l.ctx, &oneListener{c: pair.Server}, session.AcceptConfig{Local: nodeA.local(true)})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	clientEst := <-dialed
	if clientEst == nil {
		t.Fatal("client session not established")
	}
	t.Cleanup(func() { _ = clientEst.Session.Close(); _ = serverEst.Session.Close() })
	peer := Peer{Node: [32]byte(serverEst.Peer.NodeID), Device: [32]byte(serverEst.Peer.DeviceID)}
	go func() {
		for {
			st, err := serverEst.Session.AcceptStream(l.ctx)
			if err != nil {
				return
			}
			if st.Open().Service != 7 {
				st.Reset(frame.CodeProtocolViolation)
				continue
			}
			go l.srv.Handle(l.ctx, peer, st)
		}
	}()
	return &link{client: clientEst.Session, server: serverEst.Session, pair: pair}
}

func fileLimiter() frame.Limiter {
	sp, _ := service.Builtin(service.Files)
	return sp.Versions[2].Limiter()
}

func (l *lab) open(k *link) *mux.Stream {
	l.t.Helper()
	st, err := k.client.OpenStream(l.ctx, 7, 2)
	if err != nil {
		l.t.Fatalf("OpenStream: %v", err)
	}
	st.SetLimiter(fileLimiter())
	l.t.Cleanup(func() { _ = st.Close() })
	return st
}

// content returns n deterministic pseudo-random bytes.
func content(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// share puts a file of data into the lab's server for the given recipients and returns its offer.
func (l *lab) share(data []byte, recipients ...*tnode) Offer {
	l.t.Helper()
	var rs [][32]byte
	for _, r := range recipients {
		rs = append(rs, r.node())
	}
	sh, err := PrepareShare(bytes.NewReader(data), int64(len(data)), "data.bin", "application/octet-stream", nodeA.node(), rs)
	if err != nil {
		l.t.Fatalf("PrepareShare: %v", err)
	}
	if err := l.srv.Add(sh); err != nil {
		l.t.Fatalf("Add: %v", err)
	}
	return sh.Offer
}

// memStore is a Store in memory.
type memStore struct {
	mu   sync.Mutex
	n    int
	size int
	data map[int][]byte
	put  []int
	done bool
}

func newMemStore(o Offer) *memStore {
	return &memStore{n: o.Chunks(), size: int(o.Size), data: map[int][]byte{}}
}

func (s *memStore) Have(i int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[i]
	return ok
}
func (s *memStore) Count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.data) }
func (s *memStore) Put(i int, d []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[i] = append([]byte(nil), d...)
	s.put = append(s.put, i)
	return nil
}
func (s *memStore) Commit() error { s.mu.Lock(); s.done = true; s.mu.Unlock(); return nil }
func (s *memStore) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	for i := 0; i < s.n; i++ {
		out = append(out, s.data[i]...)
	}
	return out
}

// fetch runs a fetch over a fresh stream and waits for it.
func (l *lab) fetch(k *link, o Offer, st Store, opt FetchOptions) error {
	l.t.Helper()
	return Fetch(l.ctx, l.open(k), o, st, opt)
}

func fast() FetchOptions { return FetchOptions{Clock: realClock{}, MinRTO: 40 * time.Millisecond} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	return time.AfterFunc(d, f)
}

func describe(err error) string { return fmt.Sprint(err) }
