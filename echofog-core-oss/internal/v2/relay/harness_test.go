package relay

import (
	"context"
	"net"
	"net/netip"
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
	"github.com/echofog/echofog-core/internal/v2/session"
)

var relayServices = []handshake.ServiceOffer{{ID: 5, Versions: []uint32{2}}, {ID: 3, Versions: []uint32{2}}}

var suites = []uint16{handshake.SuiteXK, handshake.SuiteXX}

// tnode is a throwaway account with one device.
type tnode struct {
	t       *testing.T
	account *identity.Account
	device  *identity.Device
	devices []byte
	now     time.Time
}

func newTNode(t *testing.T) *tnode {
	t.Helper()
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatalf("identity.NewThrowaway: %v", err)
	}
	t.Cleanup(tw.Destroy)
	devices, err := tw.DeviceList(1, time.Now())
	if err != nil {
		t.Fatalf("DeviceList: %v", err)
	}
	return &tnode{t: t, account: tw.Account, device: tw.Device, devices: devices, now: time.Now()}
}

func (n *tnode) card(scope objects.Scope, seq uint64) []byte {
	n.t.Helper()
	body := &pb.NodeCard{ProtoVer: handshake.ProtoVer, HsKey: n.device.Handshake().Public(),
		L1Suites: []uint32{uint32(handshake.SuiteXK), uint32(handshake.SuiteXX)},
		Account:  n.account.Public(), Devices: n.devices, Scope: uint32(scope)}
	var env []byte
	err := n.device.WithSigner(func(s *keys.Signer) error {
		var err error
		env, err = objects.SealCard(s, seq, uint64(n.now.Add(24*time.Hour).Unix()), body)
		return err
	})
	if err != nil {
		n.t.Fatalf("SealCard: %v", err)
	}
	return env
}

func (n *tnode) own() (handshake.OwnCards, []byte) {
	peer := n.card(objects.ScopePeer, 10)
	return handshake.OwnCards{
		Current: map[uint32]handshake.OwnCard{uint32(objects.ScopePeer): {Seq: 10, Envelope: peer}},
		Issued:  map[uint64]handshake.IssuedCard{10: {Scope: uint32(objects.ScopePeer), At: n.now}},
	}, peer
}

func (n *tnode) local(withOwn bool) session.LocalIdentity {
	own, _ := n.own()
	l := session.LocalIdentity{Device: n.device, Card: n.card(objects.ScopePeer, 7), Suites: suites,
		Features: []uint32{uint32(frame.FeaturePathMigration)}, Services: relayServices}
	if withOwn {
		l.OwnCards = own
	}
	return l
}

func (n *tnode) verified() *objects.Card {
	_, peer := n.own()
	c, err := objects.VerifyCard(peer, n.now)
	if err != nil {
		n.t.Fatalf("VerifyCard: %v", err)
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

var _ carrier.Listener = (*oneListener)(nil)

// side is a node that holds a session with R.
type side struct {
	w       *world
	n       *tnode
	node    [32]byte
	toR     *mux.Session // this side's session with R
	hub     *Hub
	in      chan Inbound
	addr    netip.AddrPort
	listed  bool
	rEst    *session.Established
	pol     func(Incoming) bool
	binder  *mux.Binder
	rejects chan struct{}
}

// world is R plus its connected nodes.
type world struct {
	t      *testing.T
	ctx    context.Context
	r      *tnode
	rNode  [32]byte
	srv    *Server
	mu     sync.Mutex
	nodes  map[[32]byte]*side
	silent map[[32]byte]bool // nodes that never answer INCOMING
	raw    map[[32]byte]func(st *mux.Stream)
}

type rTarget struct {
	s *side
	w *world
}

func (r rTarget) OpenStream(ctx context.Context) (*mux.Stream, error) {
	return r.s.rEst.Session.OpenStream(ctx, 5, 2)
}
func (r rTarget) Listed() bool { return r.s.listed }
func (r rTarget) Peer() Peer   { return r.w.peerOf(r.s) }

func (w *world) peerOf(s *side) Peer {
	return Peer{Node: s.node, SID: s.rEst.SID, Remote: s.addr,
		SendDatagram: func(typ uint64, p []byte) error { return s.rEst.Session.SendDatagram(typ, p) }}
}

func (w *world) Lookup(node [32]byte) (Target, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.nodes[node]
	if !ok {
		return nil, false
	}
	return rTarget{s, w}, true
}

func newWorld(t *testing.T, cfg ServerConfig) *world {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	w := &world{t: t, ctx: ctx, r: newTNode(t), nodes: map[[32]byte]*side{}, silent: map[[32]byte]bool{}, raw: map[[32]byte]func(*mux.Stream){}}
	cfg.Directory = w
	if cfg.Delay == nil {
		cfg.Delay = func() time.Duration { return 150 * time.Millisecond }
	}
	w.srv = NewServer(cfg)
	return w
}

// join connects a new node to R and starts both serving loops.
func (w *world) join(n *tnode, ip string, listed bool) *side {
	t := w.t
	t.Helper()
	pair := memcarrier.NewBytes()
	s := &side{w: w, n: n, hub: NewHub(), in: make(chan Inbound, 8), listed: listed,
		addr: netip.MustParseAddrPort(ip + ":4000"), pol: func(Incoming) bool { return true }}
	_, rCardEnv := w.r.own()
	rCard, err := objects.VerifyCard(rCardEnv, w.r.now)
	if err != nil {
		t.Fatal(err)
	}
	dialed := make(chan *session.Established, 1)
	go func() {
		est, err := session.Dial(w.ctx, rCard, carrier.Attempt{Kind: carrier.AttemptCarrier}, n.local(false),
			session.WithCarrier(pair.Client))
		if err != nil {
			t.Errorf("dial R: %v", err)
		}
		dialed <- est
	}()
	rEst, err := session.Accept(w.ctx, &oneListener{c: pair.Server}, session.AcceptConfig{Local: w.r.local(true)})
	if err != nil {
		t.Fatalf("R accept: %v", err)
	}
	est := <-dialed
	if est == nil {
		t.FailNow()
	}
	t.Cleanup(func() { _ = est.Session.Close(); _ = rEst.Session.Close() })
	s.rEst, s.toR = rEst, est.Session
	s.node = rEst.Peer.NodeID
	w.rNode = est.Peer.NodeID
	w.mu.Lock()
	w.nodes[s.node] = s
	w.mu.Unlock()

	// R serves relay/2 streams and RELAY_DGRAM from the node.
	go func() {
		for {
			st, err := rEst.Session.AcceptStream(w.ctx)
			if err != nil {
				return
			}
			if st.Open().Service != 5 {
				st.Reset(frame.CodeProtocolViolation)
				continue
			}
			go w.srv.Handle(w.ctx, w.peerOf(s), st)
		}
	}()
	go func() {
		for {
			d, err := rEst.Session.ReceiveDatagram(w.ctx)
			if err != nil {
				return
			}
			if d.Type == frame.DgramRelay {
				w.srv.HandleDatagram(w.peerOf(s), d.Payload)
			}
		}
	}()
	// The node answers INCOMING streams from R and datagrams from R.
	go func() {
		for {
			st, err := est.Session.AcceptStream(w.ctx)
			if err != nil {
				return
			}
			if w.silentNode(s.node) {
				continue // never answers
			}
			if f := w.rawHandler(s.node); f != nil {
				go f(st)
				continue
			}
			go func() {
				in, err := Answer(st, w.rNode, s.hub, func(typ uint64, p []byte) error { return est.Session.SendDatagram(typ, p) }, s.pol)
				if err == nil {
					s.in <- in
				}
			}()
		}
	}()
	go func() {
		for {
			d, err := est.Session.ReceiveDatagram(w.ctx)
			if err != nil {
				return
			}
			if d.Type == frame.DgramRelay {
				s.hub.HandleDatagram(d.Payload)
			}
		}
	}()
	return s
}

func (w *world) silentNode(n [32]byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.silent[n]
}

func (s *side) open(ctx context.Context) (*mux.Stream, error) { return s.toR.OpenStream(ctx, 5, 2) }

// dial opens a stream circuit from s to b through R.
func (s *side) dial(b *side) (*Circuit, error) {
	return Dial(s.w.ctx, s.open, s.w.rNode, b.node, ConnectOptions{})
}

// relayedSession runs A's handshake with B inside a circuit through R.
func (w *world) relayedSession(a, b *side) (*session.Established, *session.Established, *mux.Binder) {
	t := w.t
	t.Helper()
	circ, err := a.dial(b)
	if err != nil {
		t.Fatalf("dial circuit: %v", err)
	}
	var in Inbound
	select {
	case in = <-b.in:
	case <-time.After(5 * time.Second):
		t.Fatal("no INCOMING at B")
	}
	binder := mux.NewBinder()
	accepted := make(chan *session.Established, 1)
	go func() {
		est, err := session.Accept(w.ctx, &oneListener{c: in.Stream}, session.AcceptConfig{
			Local: b.n.local(true), Kind: mux.PathRelay, Binder: binder,
			OnEstablished: func(s *mux.Session) { binder.Add(s) }})
		if err != nil {
			t.Errorf("B accept: %v", err)
		}
		accepted <- est
	}()
	aEst, err := session.Dial(w.ctx, b.n.verified(), carrier.Attempt{Kind: carrier.AttemptCarrier}, a.n.local(false),
		session.WithCarrier(circ), session.WithPathKind(mux.PathRelay))
	if err != nil {
		t.Fatalf("A dial through R: %v", err)
	}
	bEst := <-accepted
	if bEst == nil {
		t.FailNow()
	}
	t.Cleanup(func() { _ = aEst.Session.Close(); _ = bEst.Session.Close() })
	return aEst, bEst, binder
}

func (w *world) rawHandler(n [32]byte) func(*mux.Stream) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.raw[n]
}
