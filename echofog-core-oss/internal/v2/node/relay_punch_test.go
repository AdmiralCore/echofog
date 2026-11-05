package node_test

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/punch"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// natWorld imitates two address-restricted cone NATs between A and B (T6.2): a datagram from X to Y
// passes Y's NAT only when Y has sent to X before. Junk from B opens B's NAT towards A's offered
// address; only then do A's Inits reach B's listener, and the flow becomes a carrier on B's queue.
type natWorld struct {
	mu      sync.Mutex
	opened  map[[2]netip.AddrPort]bool // [from, to]: from has sent to
	bAddr   netip.AddrPort
	aAddr   netip.AddrPort
	bq      *carrierQueue
	dials   [][]netip.AddrPort
	junkTo  []netip.AddrPort
	carrier mux.Carrier
}

type natNet struct {
	w    *natWorld
	role punch.Role
}

func (n natNet) OwnAddrs() []netip.AddrPort {
	if n.role == punch.Initiator {
		return []netip.AddrPort{n.w.aAddr}
	}
	return []netip.AddrPort{n.w.bAddr}
}

func (n natNet) SendJunk(to netip.AddrPort, _ []byte) error {
	n.w.mu.Lock()
	defer n.w.mu.Unlock()
	n.w.opened[[2]netip.AddrPort{n.w.bAddr, to}] = true
	n.w.junkTo = append(n.w.junkTo, to)
	return nil
}

func (n natNet) Dial(ctx context.Context, targets []netip.AddrPort, _ int) (mux.Carrier, error) {
	n.w.mu.Lock()
	n.w.dials = append(n.w.dials, append([]netip.AddrPort(nil), targets...))
	n.w.mu.Unlock()
	for ctx.Err() == nil {
		n.w.mu.Lock()
		for _, t := range targets {
			// A's Init to t reaches B only if B has already sent to A's mapped address.
			if t == n.w.bAddr && n.w.opened[[2]netip.AddrPort{n.w.bAddr, n.w.aAddr}] {
				pair := memcarrier.NewQUIC()
				n.w.mu.Unlock()
				n.w.bq.push(pair.Server)
				return pair.Client, nil
			}
		}
		n.w.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	return nil, ctx.Err()
}

type natNetwork struct{ w *natWorld }

func (n natNetwork) Net(_ node.DeviceID, role punch.Role) punch.Net { return natNet{n.w, role} }

// publicListener puts a noise3 listener with a public address into the card, so the peer's punch
// rules know the IP (services.md §6.4 step 2).
func publicListener(host string, port uint32) func(*cards.Config) {
	return func(c *cards.Config) {
		c.Listeners[0].Host, c.Listeners[0].Port = host, port
	}
}

func relayRoleCards(c *cards.Config) {
	c.Role = objects.RoleServer
	c.Flags |= objects.FlagRelay
	c.Listeners[0].Host = "192.0.2.1"
}

type relayWorld struct {
	r, a, b *testNode
	nat     *natWorld
	mod     *fakeModule
	cancel  context.CancelFunc
	ctx     context.Context
}

// newRelayWorld starts R (relay), A and B; A and B each hold an outgoing session with R. B listens on
// bq for a punched carrier.
func newRelayWorld(t *testing.T, punchNet bool) *relayWorld {
	t.Helper()
	nat := &natWorld{opened: map[[2]netip.AddrPort]bool{},
		bAddr: netip.MustParseAddrPort("203.0.113.5:61000"), aAddr: netip.MustParseAddrPort("198.51.100.9:50000"),
		bq: newCarrierQueue()}
	rq := newCarrierQueue()
	setNet := func(own string) func(c *node.Config) {
		return func(c *node.Config) {
			if punchNet {
				c.PunchNet = natNetwork{nat}
				pathFeatures(c)
				// One country for everybody: noise3 is allowed between the two (D21).
				c.Country = func(netip.Addr) (string, bool) { return "ZZ", true }
				c.Profile.LocalAddrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr(own)} }
			}
		}
	}
	r := newTestNodeFull(t, cards.Visible, false, []carrier.Listener{rq}, nil, relayRoleCards, func(c *node.Config) {
		c.Profile = node.Profile{Platform: node.PlatformServer, Roles: []node.Role{node.RoleRelay}}
	})
	a := newTestNodeFull(t, cards.Visible, false, nil, nil, publicListener("198.51.100.9", 50000), setNet("198.51.100.9"))
	b := newTestNodeFull(t, cards.Visible, false, []carrier.Listener{nat.bq}, nil, publicListener("203.0.113.5", 61000), setNet("203.0.113.5"))
	mod := &fakeModule{got: make(chan node.Peer, 4), errs: make(chan error, 4)}
	registerFakeModule(t, a.rt, mod)
	registerFakeModule(t, b.rt, mod)
	r.start(t)
	a.start(t)
	b.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)

	for _, n := range []*testNode{a, b} {
		pair := memcarrier.NewQUIC()
		rq.push(pair.Server)
		if _, _, err := n.rt.Session(ctx, r.id.DeviceID, node.DialOpt{Card: r.peerCard(t), Carrier: pair.Client}); err != nil {
			t.Fatalf("dial R: %v", err)
		}
		// rq holds one carrier at a time: wait until R took it.
		time.Sleep(50 * time.Millisecond)
	}
	return &relayWorld{r: r, a: a, b: b, nat: nat, mod: mod, ctx: ctx, cancel: cancel}
}

func (w *relayWorld) openChat(t *testing.T) node.Peer {
	t.Helper()
	p, st, err := w.a.rt.Open(w.ctx, w.b.id.DeviceID, service.Chat, node.DialOpt{Card: w.b.peerCard(t), ViaRelay: true})
	if err != nil {
		t.Fatalf("Open through R: %v", err)
	}
	if err := st.WriteFrame(0x40, 0, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	f, err := st.ReadFrame()
	if err != nil || f.Type != 0x41 || string(f.Payload) != "pong" {
		t.Fatalf("echo through the relay: %v %v", f, err)
	}
	_ = st.Close()
	return p
}

// A reaches B through R (N1: the relay client is wired into the node): the session is relayed, B's
// module sees A, and R carries only the sealed circuit.
func TestSessionThroughRelay(t *testing.T) {
	w := newRelayWorld(t, false)
	p := w.openChat(t)
	if p.Path != mux.PathRelay {
		t.Fatalf("path %v, want relay", p.Path)
	}
	select {
	case got := <-w.mod.got:
		if got.Device != w.a.id.DeviceID || got.Path != mux.PathRelay {
			t.Fatalf("B sees %x on %v", got.Device, got.Path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B's module saw nothing")
	}
}

// Cone NAT on both sides: the relayed session gets a direct path and moves to it; the targets are
// only the addresses the rules allow.
func TestRelayedSessionPunchesToDirectPath(t *testing.T) {
	w := newRelayWorld(t, true)
	w.openChat(t)
	waitUntil(t, 15*time.Second, "a second path on A", func() bool {
		ids, _, ok := w.a.rt.Paths(w.b.id.DeviceID)
		return ok && len(ids) >= 2
	})
	ids, active, _ := w.a.rt.Paths(w.b.id.DeviceID)
	if len(ids) < 2 || active == ids[0] {
		t.Fatalf("paths %v active %d: the session did not move to the direct path", ids, active)
	}
	w.nat.mu.Lock()
	defer w.nat.mu.Unlock()
	if len(w.nat.dials) == 0 {
		t.Fatal("no Init dial")
	}
	for _, targets := range w.nat.dials {
		if len(targets) != 1 || targets[0] != w.nat.bAddr {
			t.Fatalf("Init targets %v, want only %v", targets, w.nat.bAddr)
		}
	}
	for _, to := range w.nat.junkTo {
		if to != w.nat.aAddr {
			t.Fatalf("junk to %v, want only A's offered %v", to, w.nat.aAddr)
		}
	}
}

// Without a PunchNet (no datagram side) nothing is punched: the session stays relayed.
func TestNoPunchWithoutNetwork(t *testing.T) {
	w := newRelayWorld(t, false)
	w.openChat(t)
	time.Sleep(500 * time.Millisecond)
	ids, _, _ := w.a.rt.Paths(w.b.id.DeviceID)
	if len(ids) != 1 {
		t.Fatalf("paths %v, want only the relay path", ids)
	}
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A hidden node is registered at its relays under rendezvous IDs only (services.md §6.3): a contact
// with the personal pass reaches it through R by the ID of the day, and R never saw the node ID in a
// CONNECT.
func TestHiddenNodeReachedThroughRendezvous(t *testing.T) {
	f := newAccessFactory()
	server, contacts, mgr := newHiddenAccessNode(t, f.build)
	_ = contacts
	rq := newCarrierQueue()
	r := newTestNodeFull(t, cards.Visible, false, []carrier.Listener{rq}, nil, relayRoleCards, func(c *node.Config) {
		c.Profile = node.Profile{Platform: node.PlatformServer, Roles: []node.Role{node.RoleRelay}}
	})
	client := newCardPushClient(t)
	mod := &fakeModule{got: make(chan node.Peer, 4), errs: make(chan error, 4)}
	registerFakeModule(t, server.rt, mod)
	r.start(t)
	server.start(t)

	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()

	// The client redeems an invitation and learns the personal pass and the peer card.
	minted, err := server.rt.MintInvitation(ctx, access.MintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	invite, err := objects.VerifyCard(minted.Card, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	first := memcarrier.NewQUIC()
	f.latest(minted.Listener).push(first.Server)
	_, sess1, err := client.rt.Session(ctx, server.id.DeviceID, node.DialOpt{Card: invite, Carrier: first.Client, AccessSecret: minted.Secret})
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	var personal *access.Info
	waitFor(t, 10*time.Second, "personal pass", func() bool { personal = personalPass(t, mgr); return personal != nil })
	var peerCard *objects.Card
	waitFor(t, 10*time.Second, "peer card", func() bool { peerCard = client.rt.Cards().Peer(server.id.DeviceID); return peerCard != nil })
	_ = sess1.Close()

	// Both connect out to R; the hidden node registers by itself.
	for _, n := range []*testNode{server, client} {
		pair := memcarrier.NewQUIC()
		rq.push(pair.Server)
		if _, _, err := n.rt.Session(ctx, r.id.DeviceID, node.DialOpt{Card: r.peerCard(t), Carrier: pair.Client}); err != nil {
			t.Fatalf("dial R: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var p node.Peer
	var st *mux.Stream
	waitUntil(t, 15*time.Second, "rendezvous session through R", func() bool {
		var err error
		p, st, err = client.rt.Open(ctx, server.id.DeviceID, service.Chat, node.DialOpt{
			Card: peerCard, AccessSecret: personal.Secret, ViaRelay: true})
		return err == nil
	})
	if p.Path != mux.PathRelay {
		t.Fatalf("path %v, want relay", p.Path)
	}
	_ = st.Close()
	select {
	case got := <-mod.got:
		if got.Device != client.id.DeviceID {
			t.Fatalf("hidden node sees %x", got.Device)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hidden node's module saw nothing")
	}
}
