package node_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// The T6.4 world: a substituted GeoIP puts 198.51.100.0/24 in RU and 203.0.113.0/24 in DE. A is in RU,
// R is a relay in RU (the home entry), B is abroad. Every direct dial of A goes through a counting
// fake suite dialer, so "no direct traffic" is a count of zero.

type dialCall struct {
	suite uint32
	host  string
}

type dialLog struct {
	mu    sync.Mutex
	calls []dialCall
}

func (l *dialLog) add(c dialCall) { l.mu.Lock(); l.calls = append(l.calls, c); l.mu.Unlock() }

func (l *dialLog) count(match func(dialCall) bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if match == nil || match(c) {
			n++
		}
	}
	return n
}

type fakeSuiteDialer struct {
	suite uint32
	log   *dialLog
	hook  func(req carrier.DialRequest)
}

func (d *fakeSuiteDialer) Dial(_ context.Context, req carrier.DialRequest) (mux.Carrier, error) {
	d.log.add(dialCall{d.suite, req.Addr.GetHost()})
	if d.hook != nil {
		d.hook(req)
	}
	return nil, errors.New("fake suite: unreachable")
}

func routeCountry(a netip.Addr) (string, bool) {
	s := a.String()
	switch {
	case strings.HasPrefix(s, "198.51.100."):
		return "RU", true
	case strings.HasPrefix(s, "203.0.113."):
		return "DE", true
	}
	return "", false
}

type routeOpts struct {
	net     rp.NetClass
	rules   *rp.Rules
	noEntry bool // R does not exist: A has no entry node
	bMutate func(*cards.Config)
	bCover  bool
	hook    func(req carrier.DialRequest)
	dialCtx func(context.Context, string, string) (net.Conn, error)
}

type routeWorld struct {
	r, a, b *testNode
	log     *dialLog
	events  *captureEvents
	ctx     context.Context
}

func newRouteWorld(t *testing.T, o routeOpts) *routeWorld {
	t.Helper()
	log := &dialLog{}
	reg := carrier.NewRegistry()
	for _, s := range []uint32{objects.SuiteNoise3, objects.SuiteReality2QUIC, objects.SuiteReality2TCP, objects.SuiteWS2} {
		reg.RegisterDialer(s, &fakeSuiteDialer{suite: s, log: log, hook: o.hook})
	}
	events := &captureEvents{}
	rq, bq := newCarrierQueue(), newCarrierQueue()
	r := newTestNodeFull(t, cards.Visible, false, []carrier.Listener{rq}, nil, func(c *cards.Config) {
		c.Role = objects.RoleServer
		c.Flags |= objects.FlagRelay
		c.Listeners[0].Host = "198.51.100.77"
	}, func(c *node.Config) {
		c.Profile = node.Profile{Platform: node.PlatformServer, Roles: []node.Role{node.RoleRelay}}
	})
	a := newTestNodeFull(t, cards.Visible, false, nil, events, nil, func(c *node.Config) {
		c.Registry, c.Country = reg, routeCountry
		c.NetClass, c.RoutePolicy = o.net, o.rules
		c.DialContext = o.dialCtx
		c.Profile.LocalAddrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("198.51.100.9")} }
	})
	b := newTestNodeFull(t, cards.Visible, o.bCover, []carrier.Listener{bq}, nil, o.bMutate, nil)
	mod := &fakeModule{got: make(chan node.Peer, 4), errs: make(chan error, 4)}
	registerFakeModule(t, b.rt, mod)
	registerFakeModule(t, a.rt, &fakeModule{got: make(chan node.Peer, 4), errs: make(chan error, 4)})
	r.start(t)
	a.start(t)
	b.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	for _, n := range []*testNode{a, b} {
		if n == a && o.noEntry {
			continue
		}
		pair := memcarrier.NewQUIC()
		rq.push(pair.Server)
		if _, _, err := n.rt.Session(ctx, r.id.DeviceID, node.DialOpt{Card: r.peerCard(t), Carrier: pair.Client}); err != nil {
			t.Fatalf("dial R: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &routeWorld{r: r, a: a, b: b, log: log, events: events, ctx: ctx}
}

func (w *routeWorld) open(t *testing.T, opt node.DialOpt) (node.Peer, error) {
	t.Helper()
	opt.Card = w.b.peerCard(t)
	p, st, err := w.a.rt.Open(w.ctx, w.b.id.DeviceID, service.Chat, opt)
	if err != nil {
		return node.Peer{}, err
	}
	_ = st.Close()
	return p, nil
}

func entryRules(t *testing.T, cidr string) *rp.Rules {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r, err := rp.Parse(rp.Sign(priv, []byte(`{"seq":1,"version":"t6.4-test","allowed_cidrs":["`+cidr+`"]}`)), pub, 0)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// RU mobile → abroad: the first hop is the home entry, and no direct dial of any suite (TCP or UDP)
// reaches the foreign address.
func TestRUMobileToForeignGoesThroughTheHomeEntry(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetMobile, rules: entryRules(t, "198.51.100.0/24"), bCover: true})
	p, err := w.open(t, node.DialOpt{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if p.Path != mux.PathRelay {
		t.Fatalf("path %v, want the relay path through the entry", p.Path)
	}
	if n := w.log.count(func(c dialCall) bool { return strings.HasPrefix(c.host, "203.0.113.") }); n != 0 {
		t.Fatalf("%d direct dials to the foreign address, want none", n)
	}
	rep, ok := w.a.rt.RouteReport(w.b.id.DeviceID)
	if !ok || rep.RuleID != "ru-mobile-foreign" || rep.TableVersion != "t6.4-test" || !rep.ViaEntry {
		t.Fatalf("report %+v %v: the connection report names the rule and the table version", rep, ok)
	}
}

// No entry node in the own country at all: a clear event and a clear error, no quiet slow path.
func TestRUMobileWithoutHomeEntryReportsIt(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetMobile, rules: entryRules(t, "198.51.100.0/24"), noEntry: true, bCover: true})
	_, err := w.open(t, node.DialOpt{})
	if !errors.Is(err, node.ErrNoDomesticEntry) {
		t.Fatalf("Open: %v, want ErrNoDomesticEntry", err)
	}
	if !w.events.has(node.EventNoDomesticEntry) {
		t.Fatal("no event for the application")
	}
	if n := w.log.count(nil); n != 0 {
		t.Fatalf("%d direct dials, want none", n)
	}
}

// H14: an entry outside the verified ranges is not an entry; with no verified range at all the
// answer is the honest "v2.0 does not support this".
func TestEntryOutsideVerifiedRangesIsNotUsed(t *testing.T) {
	for _, c := range []struct {
		name  string
		rules *rp.Rules
		want  error
	}{
		{"range elsewhere", entryRules(t, "10.0.0.0/8"), node.ErrNoDomesticEntry},
		{"built-in table, no verified range", nil, node.ErrRouteUnsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newRouteWorld(t, routeOpts{net: rp.NetMobile, rules: c.rules, bCover: true})
			_, err := w.open(t, node.DialOpt{})
			if !errors.Is(err, c.want) {
				t.Fatalf("Open: %v, want %v", err, c.want)
			}
			if !w.events.has(node.EventNoDomesticEntry) {
				t.Fatal("no event for the application")
			}
			if n := w.log.count(nil); n != 0 {
				t.Fatalf("%d direct dials, want none", n)
			}
		})
	}
}

// The same client on a wired network (or with the class unknown) is not restricted: the foreign
// address is dialed directly first.
func TestWiredAndUnknownNetworksDialDirectly(t *testing.T) {
	for _, nc := range []rp.NetClass{rp.NetWired, rp.NetUnknown} {
		t.Run(nc.String(), func(t *testing.T) {
			w := newRouteWorld(t, routeOpts{net: nc, bCover: true})
			if _, err := w.open(t, node.DialOpt{}); err != nil {
				t.Fatalf("Open: %v", err) // the fake suite fails, the relay is the fallback
			}
			if n := w.log.count(func(c dialCall) bool { return c.host == "203.0.113.10" }); n == 0 {
				t.Fatal("no direct dial to the foreign address on a wired network")
			}
		})
	}
}

// RU mobile → a node in the own country: the table row allows every suite, no entry needed.
func TestRUMobileToDomesticDialsDirectly(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetMobile, bMutate: func(c *cards.Config) { c.Listeners[0].Host = "198.51.100.50" }})
	if _, err := w.open(t, node.DialOpt{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if n := w.log.count(func(c dialCall) bool { return c.suite == objects.SuiteNoise3 && c.host == "198.51.100.50" }); n == 0 {
		t.Fatal("no direct noise3 dial inside the own country")
	}
}

// The probe: a direct dial that the policy forbids for traffic, TCP suites only, and the socket
// refuses to carry more than the 12 KiB budget.
func TestProbeIsTCPOnlyAndBudgeted(t *testing.T) {
	var wrote int
	var werr error
	hook := func(req carrier.DialRequest) {
		if req.Addr.GetSuite() != objects.SuiteReality2TCP || req.DialContext == nil {
			return
		}
		c, err := req.DialContext(context.Background(), "tcp", "203.0.113.10:443")
		if err != nil {
			werr = err
			return
		}
		defer c.Close()
		buf := make([]byte, 1024)
		for i := 0; i < 40; i++ {
			n, err := c.Write(buf)
			wrote += n
			if err != nil {
				werr = err
				return
			}
		}
	}
	srv, cli := net.Pipe()
	go func() { // drain the far end
		buf := make([]byte, 4096)
		for {
			if _, err := srv.Read(buf); err != nil {
				return
			}
		}
	}()
	defer cli.Close()
	w := newRouteWorld(t, routeOpts{net: rp.NetMobile, rules: entryRules(t, "198.51.100.0/24"), hook: hook, dialCtx: func(context.Context, string, string) (net.Conn, error) { return cli, nil },
		bMutate: func(c *cards.Config) {
			c.Listeners = append(c.Listeners, cards.Listener{Suite: objects.SuiteReality2TCP, Host: "203.0.113.10", Port: 443, SNI: "example.com", Mask: c.Listeners[0].Mask},
				cards.Listener{Suite: objects.SuiteReality2QUIC, Host: "203.0.113.10", Port: 443, SNI: "example.com", Mask: c.Listeners[0].Mask})
		}})
	_, _ = w.open(t, node.DialOpt{Probe: true})
	if n := w.log.count(func(c dialCall) bool { return c.suite == objects.SuiteReality2TCP }); n == 0 {
		t.Fatal("the probe did not try a TCP suite")
	}
	if n := w.log.count(func(c dialCall) bool { return c.suite == objects.SuiteReality2QUIC || c.suite == objects.SuiteNoise3 }); n != 0 {
		t.Fatalf("the probe tried a UDP suite %d times", n)
	}
	if wrote > rp.ProbeBytes || werr == nil {
		t.Fatalf("probe socket wrote %d bytes, err %v: it must stop at %d", wrote, werr, rp.ProbeBytes)
	}
}

func TestSetNetClass(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, bCover: true})
	if w.a.rt.NetClass() != rp.NetWired {
		t.Fatal("initial class from the config")
	}
	w.a.rt.SetNetClass(rp.NetMobile)
	if w.a.rt.NetClass() != rp.NetMobile {
		t.Fatal("SetNetClass")
	}
}
