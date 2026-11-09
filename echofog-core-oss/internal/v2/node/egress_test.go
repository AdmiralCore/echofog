package node_test

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	objpb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/portmap"
	"github.com/echofog/echofog-core/internal/v2/punch"
	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
)

// fakeSocks is a SOCKS5 proxy (no authentication, CONNECT only) that records what it was asked to
// reach. With refuse set it answers "connection refused" to every request.
type fakeSocks struct {
	ln     net.Listener
	refuse atomic.Bool
	// redirect, when set, is where every CONNECT really goes (the names of the test are not real).
	redirect string
	mu       sync.Mutex
	asked    []string
}

func newFakeSocks(t *testing.T) *fakeSocks {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeSocks{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *fakeSocks) addr() string { return p.ln.Addr().String() }

func (p *fakeSocks) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

func (p *fakeSocks) serve(c net.Conn) {
	defer c.Close()
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	_, _ = c.Write([]byte{5, 0})
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		var b [4]byte
		_, _ = io.ReadFull(c, b[:])
		host = net.IP(b[:]).String()
	case 3:
		var l [1]byte
		_, _ = io.ReadFull(c, l[:])
		b := make([]byte, l[0])
		_, _ = io.ReadFull(c, b)
		host = string(b)
	case 4:
		var b [16]byte
		_, _ = io.ReadFull(c, b[:])
		host = net.IP(b[:]).String()
	default:
		return
	}
	var pb [2]byte
	_, _ = io.ReadFull(c, pb[:])
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb[:]))))
	p.mu.Lock()
	p.asked = append(p.asked, target)
	p.mu.Unlock()
	if p.refuse.Load() {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	real := target
	if p.redirect != "" {
		real = p.redirect
	}
	up, err := net.DialTimeout("tcp", real, 3*time.Second)
	if err != nil {
		_, _ = c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func egressTo(p *fakeSocks, tor bool) *node.Egress { return &node.Egress{Proxy: p.addr(), Tor: tor} }

// A node behind a proxy opens no listener and does not punch: both would show its address.
func TestEgressRefusesListenersAndPunching(t *testing.T) {
	px := newFakeSocks(t)
	for name, mut := range map[string]func(*node.Config){
		"listener": func(c *node.Config) { c.Listeners = []carrier.Listener{newCarrierQueue()} },
		"punch":    func(c *node.Config) { c.PunchNet = punchStub{} },
		"own dial": func(c *node.Config) {
			c.DialContext = func(context.Context, string, string) (net.Conn, error) { return nil, nil }
		},
		"bad proxy": func(c *node.Config) { c.Egress = &node.Egress{Proxy: "not-an-address"} },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() != nil {
					t.Fatal("panic")
				}
			}()
			if _, err := tryNewNode(t, func(c *node.Config) { c.Egress = egressTo(px, false); mut(c) }); err == nil {
				t.Fatal("node.New accepted a configuration that would show the address")
			}
		})
	}
}

type punchStub struct{}

func (punchStub) Net(node.DeviceID, punch.Role) punch.Net { return nil }

// Calls go through a relay only, whatever call_route says: a direct media path is UDP to the peer.
func TestEgressForcesRelayCalls(t *testing.T) {
	px := newFakeSocks(t)
	for _, tor := range []bool{false, true} {
		a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) { c.Egress = egressTo(px, tor) })
		if got := a.rt.CallRoute(); got != route.Relay {
			t.Fatalf("tor=%v: call route %v at start, want relay", tor, got)
		}
		a.rt.SetCallRoute(route.Direct)
		a.rt.SetCallRoute(route.Auto)
		if got := a.rt.CallRoute(); got != route.Relay {
			t.Fatalf("tor=%v: call route %v after set, want relay", tor, got)
		}
	}
	b := newTestNode(t, cards.Hidden, false, nil, nil)
	b.rt.SetCallRoute(route.Direct)
	if b.rt.CallRoute() != route.Direct {
		t.Fatal("a node without a proxy lost its call route")
	}
}

// The status says what runs.
func TestEgressStatus(t *testing.T) {
	px := newFakeSocks(t)
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) { c.Egress = egressTo(px, true) })
	st := a.rt.EgressStatus()
	if !st.Active || !st.Tor || st.Proxy != px.addr() {
		t.Fatalf("status %+v", st)
	}
	b := newTestNode(t, cards.Hidden, false, nil, nil)
	if b.rt.EgressStatus().Active {
		t.Fatal("a node without a proxy reports one")
	}
}

// Host.HTTP() goes through the proxy (names unresolved), checks the site's certificate and is not
// the standard library's client with its environment proxy.
func TestEgressHTTPGoesThroughTheProxy(t *testing.T) {
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer site.Close()
	px := newFakeSocks(t)
	roots := x509.NewCertPool()
	roots.AddCert(site.Certificate())
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) {
		c.Egress = egressTo(px, false)
		c.Egress.RootCAs = roots
	})
	cl := a.rt.HTTP()
	if cl == nil {
		t.Fatal("no HTTP client behind a proxy")
	}
	_, port, _ := net.SplitHostPort(site.Listener.Addr().String())
	px.redirect = site.Listener.Addr().String()
	resp, err := cl.Get("https://example.com:" + port + "/") // the test certificate is valid for example.com
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "ok" {
		t.Fatalf("body %q", b)
	}
	asked := px.requests()
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "example.com:") {
		t.Fatalf("proxy was asked for %v, want the name example.com sent unresolved", asked)
	}
	// Without the test root the same site is refused: the certificate is checked.
	c := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) { c.Egress = egressTo(px, false) })
	if _, err := c.rt.HTTP().Get("https://example.com:" + port + "/"); err == nil {
		t.Fatal("the client accepted a certificate its roots do not know")
	}
}

// The leak test. The peer's card lists the suites at addresses that are real sockets of the test: a
// TCP and a UDP sink. Behind the proxy the node asks the proxy for the TCP suite and sends nothing
// to either sink on its own (no UDP, no direct TCP); with the proxy refusing it connects to no one
// (fail closed).
func TestEgressNoSocketBypassesTheProxy(t *testing.T) {
	tcpSink, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpSink.Close()
	var direct atomic.Int32
	go func() {
		for {
			c, err := tcpSink.Accept()
			if err != nil {
				return
			}
			direct.Add(1)
			c.Close()
		}
	}()
	udpSink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udpSink.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := udpSink.ReadFromUDP(buf); err != nil {
				return
			}
			direct.Add(1)
		}
	}()
	tcpPort := uint32(tcpSink.Addr().(*net.TCPAddr).Port)
	udpPort := uint32(udpSink.LocalAddr().(*net.UDPAddr).Port)

	for _, tor := range []bool{false, true} {
		px := newFakeSocks(t)
		px.refuse.Store(true) // the proxy only records: whatever reaches a sink came around it
		b := newTestNodeFull(t, cards.Visible, true, nil, nil, func(c *cards.Config) {
			m := c.Listeners[0].Mask
			c.Listeners = []cards.Listener{
				{Suite: objects.SuiteNoise3, Host: "127.0.0.1", Port: udpPort, Mask: m},
				{Suite: objects.SuiteReality2QUIC, Host: "127.0.0.1", Port: udpPort, SNI: "cover.example", Mask: m},
				{Suite: objects.SuiteReality2TCP, Host: "127.0.0.1", Port: tcpPort, SNI: "cover.example", Mask: m},
			}
		}, nil)
		a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) {
			c.Egress = egressTo(px, tor)
			c.NetClass, c.RoutePolicy = rp.NetWired, nil
		})
		a.start(t)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		_, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{Card: b.peerCard(t), Attempts: allSuiteAttempts(t, udpPort, tcpPort)})
		cancel()
		if err == nil {
			_ = sess.Close()
			t.Fatalf("tor=%v: a session opened to a sink", tor)
		}
		if n := direct.Load(); n != 0 {
			t.Fatalf("tor=%v: %d packets or connections reached the peer's address without the proxy", tor, n)
		}
		asked := px.requests()
		if len(asked) == 0 {
			t.Fatalf("tor=%v: the TCP suite was not tried through the proxy (%v)", tor, err)
		}
		for _, a := range asked {
			if !strings.HasSuffix(a, ":"+strconv.Itoa(int(tcpPort))) {
				t.Fatalf("tor=%v: the proxy was asked for %s, which is not a TCP suite address", tor, a)
			}
		}

		// Fail closed: with the proxy gone altogether nothing else is tried.
		_ = px.ln.Close()
		ctx2, cancel2 := context.WithTimeout(context.Background(), 8*time.Second)
		_, sess2, err2 := a.rt.Session(ctx2, b.id.DeviceID, node.DialOpt{Card: b.peerCard(t), Attempts: allSuiteAttempts(t, udpPort, tcpPort)})
		cancel2()
		if err2 == nil {
			_ = sess2.Close()
			t.Fatalf("tor=%v: a session opened with the proxy gone", tor)
		}
		if n := direct.Load(); n != 0 {
			t.Fatalf("tor=%v: with the proxy gone, %d sockets reached the peer directly", tor, n)
		}
		if errors.Is(err2, context.DeadlineExceeded) {
			t.Fatalf("tor=%v: fail closed must be an error, not a wait: %v", tor, err2)
		}
	}
}

// allSuiteAttempts is a plan with every direct suite: the UDP ones at the UDP sink, the TCP ones at
// the TCP sink. Behind a proxy only the TCP attempts may survive.
func allSuiteAttempts(t *testing.T, udpPort, tcpPort uint32) []carrier.Attempt {
	t.Helper()
	params, err := objects.Marshal(&objpb.SuiteParams{Sni: "cover.example"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(suite uint32, port uint32) carrier.Attempt {
		return carrier.Attempt{Kind: carrier.AttemptCarrier, Suite: suite,
			Addr: &objpb.Addr{Suite: suite, Host: "127.0.0.1", Port: port, Mask: 1, Params: params}}
	}
	return []carrier.Attempt{mk(objects.SuiteNoise3, udpPort), mk(objects.SuiteReality2QUIC, udpPort), mk(objects.SuiteReality2TCP, tcpPort)}
}

// The second layer: whatever the plan holds, only the TCP suites leave the filter, and the opener
// the node hands to every carrier refuses a UDP socket.
func TestEgressFilterAndOpenerRefuseUDP(t *testing.T) {
	mk := func(s uint32) carrier.Attempt { return carrier.Attempt{Kind: carrier.AttemptCarrier, Suite: s} }
	got := node.TCPOnly([]carrier.Attempt{mk(objects.SuiteNoise3), mk(objects.SuiteReality2QUIC), mk(objects.SuiteReality2TCP), mk(objects.SuiteWS2),
		{Kind: carrier.AttemptRelay}})
	if len(got) != 3 || got[0].Suite != objects.SuiteReality2TCP || got[1].Suite != objects.SuiteWS2 || got[2].Kind != carrier.AttemptRelay {
		t.Fatalf("filter left %+v", got)
	}
	px := newFakeSocks(t)
	dial, err := node.EgressDialer(&node.Egress{Proxy: px.addr()})
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"udp", "udp4", "ip"} {
		if _, err := dial(context.Background(), network, "192.0.2.1:443"); !errors.Is(err, node.ErrEgressNetwork) {
			t.Fatalf("%s: %v, want ErrEgressNetwork", network, err)
		}
	}
	if len(px.requests()) != 0 {
		t.Fatal("a refused socket still reached the proxy")
	}
}

// The profile may ask for port mapping, but behind a proxy nothing is forwarded (T11.6a M2): the
// node builds its manager disabled and the gateway is never touched, whatever the profile says.
func TestEgressForbidsPortMapping(t *testing.T) {
	g := &pcpRouter{}
	px := newFakeSocks(t)
	n := newTestNodeFull(t, cards.Visible, true, nil, nil, nil, func(c *node.Config) {
		c.Profile.PortMapping = true
		c.PortMap = g.config()
		c.Egress = egressTo(px, false)
	})
	if node.PortMappingOf(n.rt) {
		t.Fatal("the profile still asks for port mapping behind a proxy")
	}
	if err := node.PortMapAdd(n.rt, portmap.TCP, 4430); !errors.Is(err, portmap.ErrDisabled) {
		t.Fatalf("the port-mapping manager of a node behind a proxy is not disabled: %v", err)
	}
	n.start(t)
	if err := n.rt.NetworkChanged(t.Context(), node.DialOpt{}); err != nil {
		t.Fatal(err)
	}
	if err := n.rt.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if g.dials.Load()+g.gateway.Load()+g.http.Load() != 0 {
		t.Fatalf("a node behind a proxy touched the gateway: dials=%d gateway=%d http=%d",
			g.dials.Load(), g.gateway.Load(), g.http.Load())
	}
}

// Behind a proxy the observed address is the exit's, not ours (T11.6a M2): a change must not reach
// the card or the operator, even when the listener follows the public address.
func TestEgressPublicAddrChangeDoesNotReachTheCard(t *testing.T) {
	ev := &captureEvents{}
	px := newFakeSocks(t)
	n := newTestNodeFull(t, cards.Visible, true, nil, ev, func(c *cards.Config) {
		c.Listeners[0].FollowPublic = true
	}, func(c *node.Config) { c.Egress = egressTo(px, false) })

	obs := node.ObserverOf(n.rt)
	obs.Observe(observation(1, "198.51.100.1", "8.8.8.8:4000"))
	obs.Observe(observation(2, "192.0.2.9", "8.8.8.8:4000"))
	if got, ok := n.rt.PublicAddr(); !ok || got != netip.MustParseAddr("8.8.8.8") {
		t.Fatalf("the observer did not decide the address: %v %v", got, ok)
	}
	if ev.has(node.EventPublicAddr) {
		t.Fatal("a node behind a proxy told the operator its address changed")
	}
	cur, err := n.builder.Current(t.Context(), objects.ScopePeer)
	if err != nil {
		t.Fatal(err)
	}
	card, err := objects.VerifyCard(cur.Envelope, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range card.Body.GetAddrs() {
		if a.GetSuite() == objects.SuiteNoise3 && a.GetHost() != "127.0.0.1" {
			t.Fatalf("the card host followed the exit address: %q", a.GetHost())
		}
	}
}
