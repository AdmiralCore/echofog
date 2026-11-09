package node_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/punch"
)

// bdSpy is the birthday side of a PunchNetwork: it records every socket it is asked to open and
// every datagram the modes try to send, and never finds a mapping. The gates must keep these at
// zero (services.md §6.4.1, "Where it is off").
type bdSpy struct {
	mu       sync.Mutex
	opened   int
	sent     int
	dialFrom int
	acceptOn int
	recv     chan punch.Datagram
}

func newBdSpy() *bdSpy { return &bdSpy{recv: make(chan punch.Datagram)} }

func (s *bdSpy) snapshot() (opened, sent, dialFrom, acceptOn int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened, s.sent, s.dialFrom, s.acceptOn
}

type bdSpyNet struct{ s *bdSpy }

var errSpyNoSocket = errors.New("spy: birthday sockets are off")

func (n bdSpyNet) OpenSock() (punch.Sock, error) {
	n.s.mu.Lock()
	n.s.opened++
	n.s.mu.Unlock()
	return nil, errSpyNoSocket
}

func (n bdSpyNet) Listener() punch.Sock { return bdSpySock{n.s} }

func (n bdSpyNet) DialFrom(context.Context, punch.Sock, netip.AddrPort, int) (mux.Carrier, error) {
	n.s.mu.Lock()
	n.s.dialFrom++
	n.s.mu.Unlock()
	return nil, errSpyNoSocket
}

func (n bdSpyNet) AcceptOn(context.Context, punch.Sock) error {
	n.s.mu.Lock()
	n.s.acceptOn++
	n.s.mu.Unlock()
	return errSpyNoSocket
}

type bdSpySock struct{ s *bdSpy }

func (s bdSpySock) Send(netip.AddrPort, []byte) error {
	s.s.mu.Lock()
	s.s.sent++
	s.s.mu.Unlock()
	return nil
}

func (s bdSpySock) Recv() <-chan punch.Datagram { return s.s.recv }
func (s bdSpySock) Close() error                { return nil }

// bdSpyPunch is a PunchNetwork that also supplies birthday sockets, so that the runtime builds the
// birthday engine of a relayed session exactly as it does in the field.
type bdSpyPunch struct {
	w   *natWorld
	spy *bdSpy
}

func (n bdSpyPunch) Net(_ node.DeviceID, role punch.Role) punch.Net { return natNet{n.w, role} }

func (n bdSpyPunch) Socks(node.DeviceID, func(mux.Carrier)) punch.SockNet { return bdSpyNet{n.spy} }

// newBirthdayRelayWorld is newRelayWorld with a birthday-capable punch network on both ends. Both
// ends report a narrow symmetric NAT, so with the gates open the first offer is answered with
// `mode = mutual` and both sides open birthday sockets (services.md §6.4.1).
func newBirthdayRelayWorld(t *testing.T, vis cards.Visibility, proxy bool) (*relayWorld, *bdSpy) {
	t.Helper()
	nat := &natWorld{opened: map[[2]netip.AddrPort]bool{},
		bAddr: netip.MustParseAddrPort("203.0.113.5:61000"), aAddr: netip.MustParseAddrPort("198.51.100.9:50000"),
		bq: newCarrierQueue()}
	rq := newCarrierQueue()
	aSpy := newBdSpy()

	birthday := func(spy *bdSpy, own string) func(c *node.Config) {
		return func(c *node.Config) {
			c.PunchNet = bdSpyPunch{nat, spy}
			c.Features = []uint32{uint32(frame.FeaturePathMigration), uint32(frame.FeatureBirthdayPunch)}
			c.Country = func(netip.Addr) (string, bool) { return "ZZ", true }
			c.NATOverride = func() punch.NATInfo {
				return punch.NATInfo{Class: punch.NATSymmetric, Lo: 40000, Hi: 41999, Samples: 5}
			}
			c.Profile.Platform = node.PlatformServer
			c.Profile.Birthday = true
			c.Profile.LocalAddrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr(own)} }
		}
	}
	aMutate := birthday(aSpy, "198.51.100.9")
	if proxy {
		aMutate = func(c *node.Config) {
			birthday(aSpy, "198.51.100.9")(c)
			c.DialContext = func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("spy: behind a proxy")
			}
		}
	}

	r := newTestNodeFull(t, cards.Visible, false, []carrier.Listener{rq}, nil, relayRoleCards, func(c *node.Config) {
		c.Profile.Platform = node.PlatformServer
		c.Profile.Roles = []node.Role{node.RoleRelay}
	})
	a := newTestNodeFull(t, vis, false, nil, nil, publicListener("198.51.100.9", 50000), aMutate)
	b := newTestNodeFull(t, cards.Visible, false, []carrier.Listener{nat.bq}, nil,
		publicListener("203.0.113.5", 61000), birthday(newBdSpy(), "203.0.113.5"))
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
		time.Sleep(50 * time.Millisecond)
	}
	return &relayWorld{r: r, a: a, b: b, nat: nat, mod: mod, ctx: ctx, cancel: cancel}, aSpy
}

// With the gates open the engine really does reach the birthday sockets, so the test below would
// notice a silent wiring mistake; in anonymous mode and behind a proxy the same attempt must open
// no birthday socket and send no birthday datagram.
func TestBirthdayOffInAnonymousModeAndProxy(t *testing.T) {
	cases := []struct {
		name  string
		vis   cards.Visibility
		proxy bool
		open  bool
	}{
		{"visible", cards.Visible, false, true},
		{"anonymous", cards.Anonymous, false, false},
		{"proxy", cards.Visible, true, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w, spy := newBirthdayRelayWorld(t, tc.vis, tc.proxy)
			w.openChat(t)
			if tc.open {
				waitUntil(t, 15*time.Second, "a birthday socket opened", func() bool {
					opened, _, _, _ := spy.snapshot()
					return opened > 0
				})
				return
			}
			// Wait until the ordinary attempt has run (the engine has answered the offer and sent
			// its Init): birthday would have had the same chance.
			waitUntil(t, 15*time.Second, "the ordinary punch attempt", func() bool {
				w.nat.mu.Lock()
				defer w.nat.mu.Unlock()
				return len(w.nat.dials) > 0
			})
			opened, sent, dialFrom, acceptOn := spy.snapshot()
			if opened != 0 || sent != 0 || dialFrom != 0 || acceptOn != 0 {
				t.Fatalf("birthday sockets opened=%d sent=%d dialFrom=%d acceptOn=%d, want none",
					opened, sent, dialFrom, acceptOn)
			}
		})
	}
}
