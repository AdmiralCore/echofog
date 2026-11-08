package scenarios

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/internal/v2/store"
	"github.com/echofog/echofog-core/testbed/behaviour"
)

// World is a set of real v2 nodes in one process: the product's own runtime, identity, store and
// card builder, with real noise3 listeners on loopback. A node can sit behind an in-process netem
// (loss, delay, MTU, cuts), which stands in for the router of the Docker stand.
type World struct {
	dir string

	mu     sync.Mutex
	nodes  []*Node
	closed bool
}

// NodeSpec describes a node.
type NodeSpec struct {
	Name       string
	Server     bool                   // the server profile: DHT server, mailbox and relay roles, role server in its card
	Listen     bool                   // a real noise3 listener on loopback
	Hidden     bool                   // a card that is not published
	Chat       bool                   // chat/2 on
	Netem      *behaviour.NetemConfig // the listener sits behind an in-process netem
	CardMutate func(*cards.Config)    // changes the card material before the builder seals it
	Mutate     func(*node.Config)     // changes the runtime configuration
}

// Node is one real node.
type Node struct {
	Name  string
	ID    node.Identity
	RT    *node.Runtime
	Netem *behaviour.Netem // non-nil when the listener sits behind a netem
	// Addr is the address the card advertises (the netem's front when there is one); empty when the
	// node does not listen.
	Addr string

	tw      *identity.Throwaway
	store   *store.Store
	builder *cards.CardBuilder
	started bool
}

// NewWorld returns an empty world; dir holds the nodes' stores.
func NewWorld(dir string) *World { return &World{dir: dir} }

// freeUDPPort asks the kernel for a loopback UDP port. The port is free the moment it is closed and
// the listener binds it at once; a second process taking it in between is the only risk.
func freeUDPPort() (int, error) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
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
	if spec.Listen {
		port, err := freeUDPPort()
		if err != nil {
			return fail(err)
		}
		bind := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		adv := bind
		if spec.Netem != nil {
			em, err := behaviour.StartNetem(bind, *spec.Netem)
			if err != nil {
				return fail(err)
			}
			n.Netem = em
			adv = em.Addr()
		}
		n.Addr = adv
		ap := netip.MustParseAddrPort(adv)
		cardListeners = append(cardListeners, cards.Listener{
			Suite: objects.SuiteNoise3, Host: ap.Addr().String(), Port: uint32(ap.Port()), Mask: tw.Device.Masking().Public(),
		})
		profile, err := shape.Builtin(shape.IDDefaultUDP)
		if err != nil {
			return fail(err)
		}
		sources = append(sources, node.Noise3Listener{Addr: bind, Masks: []noise3.MaskKey{maskKey}, Profile: profile})
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
	if spec.CardMutate != nil {
		spec.CardMutate(&cardCfg)
	}
	n.builder, err = cards.New(cardCfg)
	if err != nil {
		return fail(fmt.Errorf("cards: %w", err))
	}
	n.store, err = store.Open(store.Options{Dir: fmt.Sprintf("%s/%s-%d", w.dir, spec.Name, len(w.nodes)), Key: tw.Account.StorageKey()})
	if err != nil {
		return fail(fmt.Errorf("store: %w", err))
	}
	cfg := node.Config{
		Account: tw.Account, Device: tw.Device, Cards: n.builder, Store: n.store, Profile: profile,
		ListenerSources: sources, Chat: spec.Chat,
		LocalInfo: carrier.LocalInfo{Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		Country:   func(netip.Addr) (string, bool) { return "ZZ", true },
	}
	if spec.Mutate != nil {
		spec.Mutate(&cfg)
	}
	n.RT, err = node.New(cfg)
	if err != nil {
		return fail(fmt.Errorf("node: %w", err))
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
	if n.Netem != nil {
		n.Netem.Close()
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
