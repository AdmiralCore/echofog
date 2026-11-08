package scenarios

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/internal/v2/store"
	"github.com/echofog/echofog-core/testbed/behaviour"
)

// shiftClock is the clock of every node in a world: the system clock plus an offset that only grows.
// A scenario skips minutes ahead to reach a timer the product keeps (the DHT republication after a
// failed first attempt is five minutes); timers still fire on real time.
type shiftClock struct{ shift atomic.Int64 }

func (c *shiftClock) Now() time.Time { return time.Now().Add(time.Duration(c.shift.Load())) }

func (c *shiftClock) AfterFunc(d time.Duration, f func()) carrier.Timer { return time.AfterFunc(d, f) }

// World is a set of real v2 nodes in one process: the product's own runtime, identity, store and
// card builder, with real noise3 listeners on loopback. A node can sit behind an in-process netem
// (loss, delay, MTU, cuts), which stands in for the router of the Docker stand.
type World struct {
	dir string
	clk shiftClock

	mu     sync.Mutex
	nodes  []*Node
	closed bool
}

// NodeSpec describes a node.
type NodeSpec struct {
	Name    string
	Server  bool                   // the server profile: DHT server, mailbox and relay roles, role server in its card
	Listen  bool                   // a real noise3 listener on loopback
	Hidden  bool                   // a card that is not published
	Chat    bool                   // chat/2 on
	Cover   bool                   // a card entry for a camouflaged address, so the node has a public card and is listed (the address is not served)
	Exit    bool                   // the product exit role: the node offers exit/2 with the node module (private targets refused)
	LabExit bool                   // the exit server of the product on a node without the exit role, with private targets allowed: the lab web server stands on loopback
	Netem   *behaviour.NetemConfig // the listener sits behind an in-process netem
	// More are the further noise3 listeners of the node (a second address of the same device, for a
	// second path), each with its own netem or none.
	More       []*behaviour.NetemConfig
	CardMutate func(*cards.Config) // changes the card material before the builder seals it
	Mutate     func(*node.Config)  // changes the runtime configuration
}

// Node is one real node.
type Node struct {
	Name  string
	ID    node.Identity
	RT    *node.Runtime
	Netem *behaviour.Netem // non-nil when the first listener sits behind a netem
	// Netems has one entry per listener, nil where there is no netem; Addrs the advertised addresses.
	Netems []*behaviour.Netem
	Addrs  []string
	// Addr is the address the card advertises (the netem's front when there is one); empty when the
	// node does not listen.
	Addr string

	// Inbox records the chat events when the node has chat.
	Inbox *Inbox

	tw     *identity.Throwaway
	app    *app.App
	access *access.Manager
	// AccessAddr is where the invitations of a hidden node listen (noise3); empty on other nodes.
	AccessAddr string
	links      *links.Service
	store      *store.Store
	builder    *cards.CardBuilder
	started    bool
}

// NewWorld returns an empty world; dir holds the nodes' stores.
func NewWorld(dir string) *World { return &World{dir: dir} }

// freeUDPPort asks the kernel for a loopback UDP port. The port is free the moment it is closed and
// the listener binds it at once; a second process taking it in between is the only risk.
func freeUDPPort(host string) (int, error) {
	pc, err := net.ListenPacket("udp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port, nil
}

// Add builds a node. It is started by StartAll.
func (w *World) Add(spec NodeSpec) (*Node, error) {
	tw, err := identity.NewThrowaway()
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	n := &Node{Name: spec.Name, tw: tw}
	fail := func(err error) (*Node, error) {
		n.close()
		return nil, err
	}
	devices, err := tw.DeviceList(1, time.Now())
	if err != nil {
		return fail(err)
	}

	var cardListeners []cards.Listener
	var sources []node.ListenerSource
	var maskKey noise3.MaskKey
	if err := tw.Device.Masking().WithPrivate(func(b []byte) error { copy(maskKey.Private[:], b); return nil }); err != nil {
		return fail(err)
	}
	// every node has its own loopback address in its own /16, so the routing tables and the diversity
	// rules of the DHT see different networks, as they do on the Internet
	host := fmt.Sprintf("127.%d.0.1", 1+len(w.nodes))
	if spec.Listen || len(spec.More) > 0 {
		profile, err := shape.Builtin(shape.IDDefaultUDP)
		if err != nil {
			return fail(err)
		}
		cfgs := []*behaviour.NetemConfig{spec.Netem}
		cfgs = append(cfgs, spec.More...)
		for _, nc := range cfgs {
			port, err := freeUDPPort(host)
			if err != nil {
				return fail(err)
			}
			bind := net.JoinHostPort(host, strconv.Itoa(port))
			adv := bind
			var em *behaviour.Netem
			if nc != nil {
				if em, err = behaviour.StartNetem(bind, *nc); err != nil {
					return fail(err)
				}
				adv = em.Addr()
			}
			n.Netems = append(n.Netems, em)
			n.Addrs = append(n.Addrs, adv)
			ap := netip.MustParseAddrPort(adv)
			cardListeners = append(cardListeners, cards.Listener{
				Suite: objects.SuiteNoise3, Host: ap.Addr().String(), Port: uint32(ap.Port()), Mask: tw.Device.Masking().Public(),
			})
			sources = append(sources, node.Noise3Listener{Addr: bind, Masks: []noise3.MaskKey{maskKey}, Profile: profile})
		}
		n.Netem, n.Addr = n.Netems[0], n.Addrs[0]
	}

	if spec.Cover {
		cardListeners = append(cardListeners, cards.Listener{
			Suite: objects.SuiteReality2QUIC, Host: host, Port: 443, SNI: "cover.lab", Mask: tw.Device.Masking().Public(),
		})
	}
	vis := cards.Visible
	if spec.Hidden {
		vis = cards.Hidden
	}
	role := uint32(objects.RoleUser)
	profile := node.DefaultProfile(node.PlatformDesktop)
	if spec.Server {
		role = objects.RoleServer
		profile = node.DefaultProfile(node.PlatformServer)
	}
	cardCfg := cards.Config{
		Account: tw.Account.Public(), Device: tw.Device, HsKey: tw.Device.Handshake().Public(),
		L1Suites: []uint32{1, 2}, Role: role, Listeners: cardListeners, Devices: devices, Visibility: vis,
	}
	if spec.Server {
		cardCfg.Flags |= objects.FlagRelay
	}
	if spec.Exit || spec.LabExit {
		cardCfg.Exit = freeExit{}
	}
	if spec.Exit {
		profile.Roles = append(profile.Roles, node.RoleExit)
	}
	if spec.CardMutate != nil {
		spec.CardMutate(&cardCfg)
	}
	n.builder, err = cards.New(cardCfg)
	if err != nil {
		return fail(fmt.Errorf("cards: %w", err))
	}
	dir, err := os.MkdirTemp(w.dir, spec.Name+"-")
	if err != nil {
		return fail(err)
	}
	n.store, err = store.Open(store.Options{Dir: dir, Key: tw.Account.StorageKey()})
	if err != nil {
		return fail(fmt.Errorf("store: %w", err))
	}
	var mgr *access.Manager
	if spec.Hidden {
		port, err := freeUDPPort(host)
		if err != nil {
			return fail(err)
		}
		n.AccessAddr = net.JoinHostPort(host, strconv.Itoa(port))
		mgr, err = access.New(access.Config{
			KV: n.store.Store(access.Namespace), Cards: n.builder, ContactsKey: tw.Device.Masking(),
			Listeners: []cards.Listener{{Suite: objects.SuiteNoise3, Host: host, Port: uint32(port)}},
		})
		if err != nil {
			return fail(fmt.Errorf("access: %w", err))
		}
		n.access = mgr
	}
	clk, err := clock.New(clock.Config{Now: w.clk.Now})
	if err != nil {
		return fail(err)
	}
	cfg := node.Config{
		Clock: clk, CarrierClock: &w.clk,
		Account: tw.Account, Device: tw.Device, Cards: n.builder, Store: n.store, Profile: profile,
		ListenerSources: sources, Chat: spec.Chat,
		// path migration is what AddPath, NetworkChanged and the punch upgrade need; no production code
		// sets it yet (finding of T15.1), so the world sets it as the node tests do
		Features:  []uint32{uint32(frame.FeaturePathMigration), uint32(frame.FeatureCardPush)},
		Access:    mgr,
		LocalInfo: carrier.LocalInfo{Addrs: []netip.Addr{netip.MustParseAddr(host)}},
		Country:   func(netip.Addr) (string, bool) { return "ZZ", true },
	}
	if spec.Chat {
		n.Inbox = newInbox()
		cfg.ChatEvents = n.Inbox.events()
	}
	if spec.Mutate != nil {
		spec.Mutate(&cfg)
	}
	n.RT, err = node.New(cfg)
	if err != nil {
		return fail(fmt.Errorf("node: %w", err))
	}
	if spec.LabExit {
		if err := registerLabExit(n.RT); err != nil {
			return fail(err)
		}
	}
	n.ID = node.Identity{NodeID: node.NodeID(tw.Account.NodeID()), DeviceID: node.DeviceID(tw.Device.ID())}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		n.close()
		return nil, errors.New("scenarios: the world is closed")
	}
	w.nodes = append(w.nodes, n)
	return n, nil
}

// StartAll starts every node that is not started.
func (w *World) StartAll(ctx context.Context) error {
	w.mu.Lock()
	nodes := append([]*Node(nil), w.nodes...)
	w.mu.Unlock()
	for _, n := range nodes {
		if err := n.Start(ctx); err != nil {
			return fmt.Errorf("start %s: %w", n.Name, err)
		}
	}
	return nil
}

// Close stops every node and netem; it may be called twice.
func (w *World) Close() {
	w.mu.Lock()
	nodes := w.nodes
	w.nodes, w.closed = nil, true
	w.mu.Unlock()
	for _, n := range nodes {
		n.close()
	}
}

// Start starts the node's runtime.
func (n *Node) Start(ctx context.Context) error {
	if n.started {
		return nil
	}
	if err := n.RT.Start(ctx); err != nil {
		return err
	}
	n.started = true
	return nil
}

// Stop stops the runtime and keeps the store, the identity and the netem, so the node can be built
// again by a scenario that restarts it.
func (n *Node) Stop(ctx context.Context) error {
	if !n.started {
		return nil
	}
	n.started = false
	return n.RT.Stop(ctx)
}

func (n *Node) close() {
	if n.RT != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = n.RT.Stop(ctx)
		cancel()
	}
	for _, em := range n.Netems {
		if em != nil {
			em.Close()
		}
	}
	if n.access != nil {
		n.access.Close()
	}
	if n.store != nil {
		_ = n.store.Close()
	}
	if n.tw != nil {
		n.tw.Destroy()
	}
}

// PeerCard is the node's own peer card.
func (n *Node) PeerCard() (*objects.Card, error) {
	env, ok := n.RT.Cards().Own(objects.ScopePeer)
	if !ok {
		return nil, errors.New("scenarios: the node has no peer card")
	}
	return objects.VerifyCard(env, time.Now())
}

// Dial opens a session from n to to, with to's peer card.
func (n *Node) Dial(ctx context.Context, to *Node) (node.Peer, *mux.Session, error) {
	card, err := to.PeerCard()
	if err != nil {
		return node.Peer{}, nil, err
	}
	return n.RT.Session(ctx, to.ID.DeviceID, node.DialOpt{Card: card})
}

// SuiteOf names the carrier suite of n's session with to; empty when there is none.
func (n *Node) SuiteOf(to *Node) string {
	for _, s := range n.RT.Status().Sessions {
		if s.Device == to.ID.DeviceID {
			switch s.Suite {
			case objects.SuiteNoise3:
				return "noise3"
			case objects.SuiteReality2QUIC:
				return "reality2-quic"
			case objects.SuiteReality2TCP:
				return "reality2-tcp"
			case objects.SuiteWS2:
				return "ws2"
			}
			return fmt.Sprintf("suite-%d", s.Suite)
		}
	}
	return ""
}

// AttemptFor is a dial attempt at the i-th noise3 address of to, for DialOpt.Attempts: it picks the
// listener (and so the netem) a path goes through.
func (n *Node) AttemptFor(to *Node, i int) (carrier.Attempt, error) {
	card, err := to.PeerCard()
	if err != nil {
		return carrier.Attempt{}, err
	}
	var noise []*pb.Addr
	for _, a := range card.Addrs() {
		if a.GetSuite() == objects.SuiteNoise3 {
			noise = append(noise, a)
		}
	}
	if i < 0 || i >= len(noise) {
		return carrier.Attempt{}, fmt.Errorf("scenarios: %s has %d noise3 addresses, not %d", to.Name, len(noise), i+1)
	}
	return carrier.Attempt{Kind: carrier.AttemptCarrier, Suite: objects.SuiteNoise3, Addr: noise[i]}, nil
}

// DialVia is Dial through the i-th noise3 address of to only.
func (n *Node) DialVia(ctx context.Context, to *Node, i int) (node.Peer, *mux.Session, error) {
	card, err := to.PeerCard()
	if err != nil {
		return node.Peer{}, nil, err
	}
	at, err := n.AttemptFor(to, i)
	if err != nil {
		return node.Peer{}, nil, err
	}
	return n.RT.Session(ctx, to.ID.DeviceID, node.DialOpt{Card: card, Attempts: []carrier.Attempt{at}})
}

// Skip moves the clock of every node in the world forward.
func (w *World) Skip(d time.Duration) { w.clk.shift.Add(int64(d)) }

var errNoExitService = errors.New("scenarios: exit/2 is not built in")
