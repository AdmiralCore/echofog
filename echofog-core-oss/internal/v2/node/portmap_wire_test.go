package node_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/portmap"
)

// udpListener is a carrier listener that only reports the UDP port it is bound to.
type udpListener struct {
	port int
	done chan struct{}
	once sync.Once
}

func newUDPListener(port int) *udpListener {
	return &udpListener{port: port, done: make(chan struct{})}
}

func (l *udpListener) Accept(ctx context.Context) (mux.Carrier, error) {
	select {
	case <-l.done:
		return nil, errors.New("closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (l *udpListener) Addr() net.Addr { return &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: l.port} }
func (l *udpListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }

var _ carrier.Listener = (*udpListener)(nil)

// pcpRouter is a home router that speaks PCP: it answers ANNOUNCE and maps every port to
// external port 41000 on 8.8.4.4.
type pcpRouter struct {
	mu        sync.Mutex
	maps      []mapReq
	announces int
	dials     atomic.Int32
	gateway   atomic.Int32 // calls of Config.PortMap.Gateway
	http      atomic.Int32
}

type mapReq struct {
	internal uint16
	lifetime uint32
}

const routerExtPort = 41000

func (g *pcpRouter) config() portmap.Config {
	return portmap.Config{
		Gateway: func() (netip.Addr, error) { g.gateway.Add(1); return netip.MustParseAddr("192.168.1.1"), nil },
		Local:   func(netip.Addr) (netip.Addr, error) { return netip.MustParseAddr("192.168.1.10"), nil },
		Dial: func(_ context.Context, remote netip.AddrPort) (portmap.PacketConn, error) {
			g.dials.Add(1)
			return &routerConn{g: g, in: make(chan []byte, 4), done: make(chan struct{})}, nil
		},
		HTTP: httpFunc(func(*http.Request) (*http.Response, error) { g.http.Add(1); return nil, errors.New("no UPnP here") }),
		SSDP: func(context.Context) ([]string, error) { g.http.Add(1); return nil, nil },
	}
}

type httpFunc func(*http.Request) (*http.Response, error)

func (f httpFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type routerConn struct {
	g    *pcpRouter
	in   chan []byte
	done chan struct{}
	once sync.Once
}

func (c *routerConn) Write(p []byte) (int, error) {
	if len(p) < 24 || p[0] != 2 {
		return len(p), nil
	}
	switch p[1] {
	case 0:
		c.g.mu.Lock()
		c.g.announces++
		c.g.mu.Unlock()
		resp := make([]byte, 24)
		resp[0], resp[1] = 2, 0x80
		c.in <- resp
	case 1:
		if len(p) != 60 {
			return len(p), nil
		}
		life := binary.BigEndian.Uint32(p[4:8])
		c.g.mu.Lock()
		c.g.maps = append(c.g.maps, mapReq{internal: binary.BigEndian.Uint16(p[40:42]), lifetime: life})
		c.g.mu.Unlock()
		resp := make([]byte, 60)
		resp[0], resp[1] = 2, 0x81
		binary.BigEndian.PutUint32(resp[4:8], life)
		copy(resp[24:], p[24:])
		binary.BigEndian.PutUint16(resp[42:44], routerExtPort)
		copy(resp[44:60], []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 8, 8, 4, 4})
		c.in <- resp
	}
	return len(p), nil
}

func (c *routerConn) Read(p []byte) (int, error) {
	select {
	case b := <-c.in:
		return copy(p, b), nil
	case <-c.done:
		return 0, io.EOF
	}
}

func (c *routerConn) Close() error { c.once.Do(func() { close(c.done) }); return nil }

func (g *pcpRouter) mapped(port uint16, lifetimeZero bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, m := range g.maps {
		if m.internal == port && (m.lifetime == 0) == lifetimeZero {
			return true
		}
	}
	return false
}

func (g *pcpRouter) announceCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.announces
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func mappedNode(t *testing.T, vis cards.Visibility, portMapping bool, g *pcpRouter, ev node.EventSink) *testNode {
	t.Helper()
	return newTestNodeFull(t, vis, vis == cards.Visible, []carrier.Listener{newUDPListener(4430)}, ev,
		func(c *cards.Config) { c.Listeners[0].FollowPublic = true },
		func(c *node.Config) {
			c.Profile.PortMapping = portMapping
			c.PortMap = g.config()
		})
}

func firstAddr(t *testing.T, n *testNode) (string, uint32) {
	t.Helper()
	cur, err := n.builder.Current(t.Context(), objects.ScopePeer)
	if err != nil {
		t.Fatal(err)
	}
	card, err := objects.VerifyCard(cur.Envelope, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a := card.Body.GetAddrs()[0]
	return a.GetHost(), a.GetPort()
}

// agree makes two peers in different /16 networks report ip:4000 (T3.4 Observer).
func agree(n *testNode, ip string) {
	obs := node.ObserverOf(n.rt)
	obs.Observe(observation(1, "198.51.100.1", ip+":4000"))
	obs.Observe(observation(2, "192.0.2.9", ip+":4000"))
}

func TestPortMappingOffSendsNothing(t *testing.T) {
	g := &pcpRouter{}
	n := mappedNode(t, cards.Visible, false, g, nil)
	n.start(t)
	agree(n, "8.8.4.4")
	if err := n.rt.NetworkChanged(t.Context(), node.DialOpt{}); err != nil {
		t.Fatal(err)
	}
	if err := n.rt.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if g.dials.Load()+g.gateway.Load()+g.http.Load() != 0 {
		t.Fatalf("port_mapping=false touched the network: dials=%d gateway=%d http=%d", g.dials.Load(), g.gateway.Load(), g.http.Load())
	}
	if _, port := firstAddr(t, n); port != 4430 {
		t.Fatalf("card port = %d", port)
	}
}

func TestMobileProfileDoesNotMapPorts(t *testing.T) {
	for _, p := range []node.Platform{node.PlatformAndroid, node.PlatformIOS} {
		if node.DefaultProfile(p).PortMapping {
			t.Errorf("%v profile enables port mapping", p)
		}
	}
}

func TestHiddenNodeDoesNotMapPorts(t *testing.T) {
	g := &pcpRouter{}
	n := mappedNode(t, cards.Hidden, true, g, nil)
	n.start(t)
	time.Sleep(50 * time.Millisecond)
	if g.dials.Load() != 0 {
		t.Fatal("a hidden node asked the router to open its listener")
	}
}

func TestListenerPortIsMappedAndPublishedAfterPeersConfirm(t *testing.T) {
	g := &pcpRouter{}
	ev := &captureEvents{}
	n := mappedNode(t, cards.Visible, true, g, ev)
	n.start(t)
	waitCond(t, "the MAP request for the listener port", func() bool { return g.mapped(4430, false) })

	// the router's answer is only a candidate: the card still names the configured port
	if host, port := firstAddr(t, n); host != "127.0.0.1" || port != 4430 {
		t.Fatalf("card before confirmation = %s:%d", host, port)
	}

	agree(n, "8.8.4.4")
	waitCond(t, "the card to carry the mapped port", func() bool { _, p := firstAddr(t, n); return p == routerExtPort })
	if host, _ := firstAddr(t, n); host != "8.8.4.4" {
		t.Fatalf("card host = %s", host)
	}
	if !ev.has(node.EventPortMapping) {
		t.Fatal("no port-mapping event")
	}
}

func TestMappingOfAnotherAddressIsNotPublished(t *testing.T) {
	g := &pcpRouter{}
	n := mappedNode(t, cards.Visible, true, g, nil)
	n.start(t)
	waitCond(t, "the MAP request", func() bool { return g.mapped(4430, false) })

	agree(n, "1.1.1.1") // peers see an IP the router did not report: double NAT
	time.Sleep(50 * time.Millisecond)
	host, port := firstAddr(t, n)
	if host != "1.1.1.1" || port != 4430 {
		t.Fatalf("card = %s:%d, want the observed IP with the configured port", host, port)
	}
}

func TestNetworkChangeDropsTheMappedPortAndAsksTheRouterAgain(t *testing.T) {
	g := &pcpRouter{}
	n := mappedNode(t, cards.Visible, true, g, nil)
	n.start(t)
	agree(n, "8.8.4.4")
	waitCond(t, "the mapped port in the card", func() bool { _, p := firstAddr(t, n); return p == routerExtPort })
	before := g.announceCount()

	if err := n.rt.NetworkChanged(t.Context(), node.DialOpt{}); err != nil {
		t.Fatal(err)
	}
	waitCond(t, "the card to fall back to the configured port", func() bool { _, p := firstAddr(t, n); return p == 4430 })
	waitCond(t, "a new gateway discovery", func() bool { return g.announceCount() > before })
}

func TestStopRemovesTheMapping(t *testing.T) {
	g := &pcpRouter{}
	n := mappedNode(t, cards.Visible, true, g, nil)
	n.start(t)
	waitCond(t, "the MAP request", func() bool { return g.mapped(4430, false) })
	if err := n.rt.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !g.mapped(4430, true) {
		t.Fatal("Stop left the port forwarded (no MAP with lifetime 0)")
	}
}
