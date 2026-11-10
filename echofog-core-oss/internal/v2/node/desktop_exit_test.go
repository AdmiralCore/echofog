package node_test

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// T10.9: a desktop turns its exit on and off while it runs, and others reach it by a link. These
// tests run two real runtimes of one process joined by the in-memory carrier. The exit knows a client
// only by the identity it uses for this exit (services.md section 7), so the owner confirms identities.

// exitDialer dials the exit through the queue of the other runtime, like memDialer does for links.
type exitDialer struct {
	rt    *node.Runtime
	queue *carrierQueue
}

func (d exitDialer) ExitClient(ctx context.Context, card *objects.Card, opt node.DialOpt) (*exit.Client, error) {
	pair := memcarrier.NewQUIC()
	d.queue.push(pair.Server)
	opt.Carrier = pair.Client
	return d.rt.ExitClient(ctx, card, opt)
}

type exitApp struct {
	nd *testNode
	a  *app.App
	ln *links.Service
	q  *carrierQueue
}

func newExitApp(t *testing.T, own, peer *carrierQueue) *exitApp {
	t.Helper()
	var ls []carrier.Listener
	if own != nil {
		ls = []carrier.Listener{own}
	}
	x := &exitApp{q: own}
	x.nd = newTestNodeWith(t, cards.Visible, true, ls, nil, func(c *node.Config) {
		c.Chat = true
		c.ChatEvents.Control = func(from [32]byte, ctl *chatpb.Control) {
			if x.a != nil {
				x.a.OnControl(from, ctl)
			}
		}
	})
	x.nd.start(t)
	ln, err := links.New(links.Config{Cards: x.nd.builder, Dialer: memDialer{x.nd.rt, peer}, Clock: x.nd.rt.Clock()})
	if err != nil {
		t.Fatal(err)
	}
	x.ln = ln
	a, err := app.New(app.Config{
		Runtime: x.nd.rt, Links: ln, Store: x.nd.rt.Store("t109-app"), Clock: x.nd.rt.Clock(),
		Self: [32]byte(x.nd.id.NodeID), Chat: x.nd.rt.Chat(),
		Exit: app.RuntimeExit(exitDialer{x.nd.rt, peer}), VPNListen: "127.0.0.1:0", ExitHost: x.nd.rt,
	})
	if err != nil {
		t.Fatal(err)
	}
	x.a = a
	t.Cleanup(func() { _ = a.VPNStop(); _ = a.ExitDisable() })
	return x
}

func (x *exitApp) hex() string { return hex.EncodeToString(x.nd.id.NodeID[:]) }

// newExitPair makes A (the client) and B (the host); B has A as a contact.
func newExitPair(t *testing.T) (a, b *exitApp) {
	t.Helper()
	qA, qB := newCarrierQueue(), newCarrierQueue()
	a = newExitApp(t, qA, qB)
	b = newExitApp(t, qB, qA)
	cl, err := a.ln.ContactLink(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := b.a.AddByLink(ctx, cl.URL); err != nil {
		t.Fatalf("B adds A: %v", err)
	}
	return a, b
}

// site is a web server on the loopback; the exit may reach it because the tests allow private targets.
func site(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			fmt.Fprint(w, "held")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "hello from the site")
	}))
	t.Cleanup(s.Close)
	return s.Listener.Addr().String()
}

// socks5 opens a tunnel through the local proxy of the VPN and returns the reply code and the connection.
func socks5(t *testing.T, listen, target string) (byte, net.Conn) {
	t.Helper()
	c, err := net.DialTimeout("tcp", listen, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var m [2]byte
	if _, err := io.ReadFull(c, m[:]); err != nil || m != [2]byte{5, 0} {
		t.Fatalf("method reply %v %v", m, err)
	}
	host, port, _ := net.SplitHostPort(target)
	ip := net.ParseIP(host).To4()
	p, _ := net.LookupPort("tcp", port)
	req := append([]byte{5, 1, 0, 1}, ip...)
	req = append(req, byte(p>>8), byte(p))
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	var h [10]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		t.Fatalf("reply: %v", err)
	}
	return h[1], c
}

// httpGet sends a request over the tunnel and returns the last line of the answer (the body).
func httpGet(t *testing.T, c net.Conn) string {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := fmt.Fprint(c, "GET / HTTP/1.0\r\nHost: site\r\n\r\n"); err != nil {
		// The exit may close the stream before the request arrives: a refusal it has already sent,
		// or a tunnel ended from the far side. There is no answer to read then, which is not the
		// page and not a failure of the test.
		return ""
	}
	r := bufio.NewReader(c)
	var last string
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			last = line
		}
		if err != nil {
			break
		}
	}
	return last
}

// fakeProxy is a proxy of our own: it answers the greeting and then does what hand says with the
// tunnel. The exit of a real one hands a tunnel out before the exit answers (fast open), so a
// refusal reaches the client after the handshake — a listener is the only way to reach that.
func fakeProxy(t *testing.T, hand func(c net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("the fake proxy listens: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		var greeting [3]byte
		if _, err := io.ReadFull(c, greeting[:]); err != nil {
			return
		}
		if _, err := c.Write([]byte{5, 0}); err != nil {
			return
		}
		hand(c)
	}()
	return l.Addr().String()
}

// A tunnel the exit closes after the handshake is a refusal, not a failure of the test: the client
// hears the reset the proxy makes when it turns it away (fast open: the reply code is already out).
func TestAClosedStreamAfterTheHandshakeIsARefusal(t *testing.T) {
	listen := fakeProxy(t, func(c net.Conn) {
		var req [10]byte
		_, _ = io.ReadFull(c, req[:]) // the request reaches the proxy, the exit closes the stream
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // the reset the proxy leaves on a refused fast open
		}
	})
	code, c := socks5(t, listen, "127.0.0.1:80")
	if c != nil {
		_ = c.Close()
		t.Fatalf("a closed tunnel came back as a connection (code %d)", code)
	}
	if code == 0 {
		t.Fatal("a closed tunnel reads as a success reply")
	}
}

// The other side of the same answer: a tunnel the exit carries is a tunnel, and the page arrives.
func TestALiveTunnelStillReadsThePage(t *testing.T) {
	listen := fakeProxy(t, func(c net.Conn) {
		var req [10]byte
		if _, err := io.ReadFull(c, req[:]); err != nil {
			return
		}
		if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
		if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
			return
		}
		_, _ = fmt.Fprint(c, "HTTP/1.0 200 OK\r\n\r\nhello from the site")
	})
	code, c := socks5(t, listen, "127.0.0.1:80")
	if code != 0 || c == nil {
		t.Fatalf("a live tunnel reads as code %d (connection %v)", code, c)
	}
	if got := httpGet(t, c); got != "hello from the site" {
		t.Fatalf("the page over a live tunnel reads %q", got)
	}
}

var bgx = context.Background()

func enable(t *testing.T, b *exitApp) app.ExitInfo {
	t.Helper()
	info, err := b.a.ExitEnable(bgx, app.ExitSettings{AllowPrivateTargets: true})
	if err != nil {
		t.Fatalf("ExitEnable: %v", err)
	}
	if info.Link == "" {
		t.Fatal("no link to the exit")
	}
	return info
}

func connect(t *testing.T, a *exitApp, link string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(bgx, 30*time.Second)
	defer cancel()
	st, err := a.a.VPNStart(ctx, link)
	if err != nil {
		t.Fatalf("VPNStart: %v", err)
	}
	return st.Listen
}

func exitWait(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("not reached: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// confirm: the client tries once, is turned away and waits; the owner confirms it.
func confirm(t *testing.T, b *exitApp, listen, target string) {
	t.Helper()
	if tunnelWorks(t, listen, target) {
		t.Fatal("an unknown client was served before the owner confirmed it")
	}
	exitWait(t, "a client waits for confirmation", func() bool { return len(b.a.ExitStatus().Pending) == 1 })
	if err := b.a.ExitApprove(b.a.ExitStatus().Pending[0].ID, "A"); err != nil {
		t.Fatalf("ExitApprove: %v", err)
	}
}

// closed reports whether the connection ends within the time: the exit closed the tunnel.
func closed(c net.Conn, within time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(within))
	_, err := c.Read(make([]byte, 1))
	if err == nil {
		return false
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return false
	}
	return true
}

// tunnelWorks opens a tunnel and asks the site for a page: false when the exit turned the client away
// (SOCKS5 answers before the exit does, the refusal shows on the first read).
func tunnelWorks(t *testing.T, listen, target string) bool {
	t.Helper()
	code, c := socks5(t, listen, target)
	if code != 0 {
		return false
	}
	return httpGet(t, c) == "hello from the site"
}

// hold opens a tunnel to a page that stays open and returns the connection once the page began.
func hold(t *testing.T, listen, target string) net.Conn {
	t.Helper()
	code, c := socks5(t, listen, target)
	if code != 0 {
		t.Fatalf("reply %d", code)
	}
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := fmt.Fprint(c, "GET /hold HTTP/1.0\r\nHost: site\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil || len(line) < 12 || line[9:12] != "200" {
		t.Fatalf("the held page did not begin: %q %v", line, err)
	}
	return c
}

// B turns the exit on and gives a link; A opens a site through it with SOCKS5 once B has confirmed it.
func TestDesktopExitCarriesTrafficToAConfirmedClient(t *testing.T) {
	a, b := newExitPair(t)
	info := enable(t, b)
	listen := connect(t, a, info.Link)
	target := site(t)
	confirm(t, b, listen, target)
	if !tunnelWorks(t, listen, target) {
		t.Fatal("the confirmed client was refused")
	}
	st := b.a.ExitStatus()
	if !st.Enabled || st.Accepted == 0 || st.Bytes == 0 {
		t.Fatalf("status %+v", st)
	}
	if len(st.Approved) != 1 || st.Approved[0].Label != "A" {
		t.Fatalf("confirmed list %+v", st.Approved)
	}
}

// The exit of a desktop serves the internet, not the owner's own network: with the settings the
// product uses, a confirmed client is still refused when it asks for a loopback or LAN address.
func TestPrivateTargetsStayClosedInTheProduct(t *testing.T) {
	a, b := newExitPair(t)
	info, err := b.a.ExitEnable(bgx, app.ExitSettings{})
	if err != nil {
		t.Fatalf("ExitEnable: %v", err)
	}
	listen := connect(t, a, info.Link)
	target := site(t)
	confirm(t, b, listen, target)
	if tunnelWorks(t, listen, target) {
		t.Fatal("the exit reached a loopback target with the default settings")
	}
}

// A node that has not turned the exit on is not an exit, and a restart leaves it off.
func TestDesktopExitIsOffUntilTurnedOn(t *testing.T) {
	a, b := newExitPair(t)
	cl, err := b.ln.ContactLink(bgx)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bgx, 30*time.Second)
	defer cancel()
	if _, err := a.a.VPNStart(ctx, cl.URL); !errors.Is(err, app.ErrNotExit) {
		t.Fatalf("VPNStart to a node that is no exit = %v, want ErrNotExit", err)
	}
	if st := b.a.ExitStatus(); st.Enabled {
		t.Fatal("the exit is on after start")
	}
	enable(t, b)
	b2, err := app.New(app.Config{Runtime: b.nd.rt, Links: b.ln, Store: b.nd.rt.Store("t109-app"), Clock: b.nd.rt.Clock(),
		Self: [32]byte(b.nd.id.NodeID), ExitHost: b.nd.rt})
	if err != nil {
		t.Fatal(err)
	}
	if b2.ExitStatus().Enabled {
		t.Fatal("a new app starts with the exit on: it must be off after a restart")
	}
}

// After the exit is turned off new streams are refused and the live ones are closed.
func TestTurningTheExitOffRefusesNewAndClosesLive(t *testing.T) {
	a, b := newExitPair(t)
	info := enable(t, b)
	listen := connect(t, a, info.Link)
	target := site(t)
	confirm(t, b, listen, target)
	live := hold(t, listen, target)
	if err := b.a.ExitDisable(); err != nil {
		t.Fatal(err)
	}
	if !closed(live, 10*time.Second) {
		t.Fatal("a live tunnel is still open after the exit was turned off")
	}
	if tunnelWorks(t, listen, target) {
		t.Fatal("a new tunnel was accepted by an exit that is off")
	}
	if st := b.a.ExitStatus(); st.Enabled {
		t.Fatal("status says on")
	}
}

// A client nobody has confirmed is refused with a policy refusal and waits; turning it down keeps it out.
func TestAnUnknownClientGetsPolicyAndWaits(t *testing.T) {
	a, b := newExitPair(t)
	info := enable(t, b)
	listen := connect(t, a, info.Link)
	target := site(t)
	if tunnelWorks(t, listen, target) {
		t.Fatal("an unknown client was served")
	}
	if st := a.a.VPNStatus(); st.LastReason != "policy" {
		t.Fatalf("the refusal reads %q, want policy (status %+v)", st.LastReason, st)
	}
	exitWait(t, "waiting", func() bool { return len(b.a.ExitStatus().Pending) == 1 })
	st := b.a.ExitStatus()
	if st.Refused == 0 {
		t.Fatalf("the host counted no refusal: %+v", st)
	}
	if err := b.a.ExitReject(st.Pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if tunnelWorks(t, listen, target) {
		t.Fatal("served after the owner turned the client down")
	}
}

// Taking a confirmed client back closes its streams and keeps it out.
func TestRevokingAConfirmedClientClosesItsStreams(t *testing.T) {
	a, b := newExitPair(t)
	info := enable(t, b)
	listen := connect(t, a, info.Link)
	target := site(t)
	confirm(t, b, listen, target)
	live := hold(t, listen, target)
	if err := b.a.ExitRevoke(b.a.ExitStatus().Approved[0].ID); err != nil {
		t.Fatal(err)
	}
	if !closed(live, 10*time.Second) {
		t.Fatal("the stream of a revoked client was not closed within 10 s")
	}
	if tunnelWorks(t, listen, target) {
		t.Fatal("a revoked client was served")
	}
}

// Anyone with the link is served only when the owner asks for it, for a time; the access can be taken
// back, and it ends by itself. Streams it admitted are closed when it ends.
func TestAnyoneWithTheLinkOnlyOnTheOwnersWordAndForATime(t *testing.T) {
	a, b := newExitPair(t)
	info := enable(t, b)
	listen := connect(t, a, info.Link)
	target := site(t)
	if tunnelWorks(t, listen, target) {
		t.Fatal("served without being confirmed and without the open access")
	}

	open, err := b.a.ExitAllowAny(bgx, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if open.Until == 0 {
		t.Fatal("open access without an end")
	}
	live := hold(t, listen, target)
	if err := b.a.ExitRevokeAny(); err != nil {
		t.Fatal(err)
	}
	if !closed(live, 10*time.Second) {
		t.Fatal("a stream admitted by the open access survived its revocation")
	}
	if tunnelWorks(t, listen, target) {
		t.Fatal("served after the access was revoked")
	}

	if _, err := b.a.ExitAllowAny(bgx, 1*time.Second); err != nil {
		t.Fatal(err)
	}
	second := hold(t, listen, target)
	if !closed(second, 5*time.Second) {
		t.Fatal("a stream was still open after the window ended")
	}
	if tunnelWorks(t, listen, target) {
		t.Fatal("served after the access expired")
	}
	if st := b.a.ExitStatus(); st.AnyUntil != 0 {
		t.Fatalf("status still shows open access until %d", st.AnyUntil)
	}
}

// The exit never sees the messenger identity of the client: not in the status, not in the confirmed list.
func TestTheMessengerIdentityDoesNotReachTheExit(t *testing.T) {
	a, b := newExitPair(t)
	info := enable(t, b)
	listen := connect(t, a, info.Link)
	target := site(t)
	confirm(t, b, listen, target)
	if !tunnelWorks(t, listen, target) {
		t.Fatal("the confirmed client was refused")
	}
	st := b.a.ExitStatus()
	var ids []string
	for _, c := range st.Clients {
		ids = append(ids, c.ID)
	}
	for _, p := range st.Approved {
		ids = append(ids, p.ID)
	}
	if len(ids) == 0 {
		t.Fatalf("no client in the status: %+v", st)
	}
	for _, id := range ids {
		if id == a.hex() {
			t.Fatal("the exit saw the messenger identity of the client")
		}
	}
}

// Turning the exit off takes the offer off the cards: a link made afterwards is no exit link.
func TestTurnedOffExitLeavesTheCards(t *testing.T) {
	a, b := newExitPair(t)
	enable(t, b)
	if err := b.a.ExitDisable(); err != nil {
		t.Fatal(err)
	}
	cl, err := b.ln.ContactLink(bgx)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bgx, 30*time.Second)
	defer cancel()
	if _, err := a.a.VPNStart(ctx, cl.URL); !errors.Is(err, app.ErrNotExit) {
		t.Fatalf("VPNStart through a link made after the exit was turned off = %v, want ErrNotExit", err)
	}
	// and it can be turned on again
	enable(t, b)
}

// The exit of a desktop starts once: a second start is a programming mistake, not a quiet restart.
func TestAHostedExitCannotBeTurnedOnTwice(t *testing.T) {
	b := newExitApp(t, nil, nil)
	if _, err := b.a.ExitEnable(bgx, app.ExitSettings{}); err != nil {
		t.Fatalf("ExitEnable: %v", err)
	}
	if _, err := b.a.ExitEnable(bgx, app.ExitSettings{}); !errors.Is(err, node.ErrExitRunning) {
		t.Fatalf("second ExitEnable = %v, want ErrExitRunning", err)
	}
}

// A node whose profile is an exit cannot switch that exit: the role is part of who the node is.
func TestAnExitOfTheProfileCannotBeTurnedOn(t *testing.T) {
	n := newTestNodeWith(t, cards.Visible, false, nil, nil, func(c *node.Config) {
		c.Profile.Roles = []node.Role{node.RoleExit}
	})
	n.start(t)
	if err := n.rt.EnableExit(node.ExitOptions{}); !errors.Is(err, node.ErrExitStatic) {
		t.Fatalf("EnableExit on a profile exit = %v, want ErrExitStatic", err)
	}
}

// rawExitStream opens an exit/2 stream under the messenger identity of the client: that identity is
// never confirmed, so the exit must turn every kind of request away.
func rawExitStream(t *testing.T, a, b *exitApp, card *objects.Card) *mux.Stream {
	t.Helper()
	if b.q == nil {
		t.Fatal("the exit has no listener queue")
	}
	pair := memcarrier.NewQUIC()
	b.q.push(pair.Server)
	ctx, cancel := context.WithTimeout(bgx, 30*time.Second)
	defer cancel()
	_, st, err := a.nd.rt.Open(ctx, b.nd.id.DeviceID, service.Exit, node.DialOpt{Card: card, Carrier: pair.Client})
	if err != nil {
		t.Fatalf("open exit/2: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// An unconfirmed client is refused the same way on every request, not only on a TCP tunnel: UDP,
// the account frame and a voucher all meet the policy refusal.
func TestAnUnconfirmedClientIsRefusedForUDPAccountAndVoucher(t *testing.T) {
	qA, qB := newCarrierQueue(), newCarrierQueue()
	a, b := newExitApp(t, qA, qB), newExitApp(t, qB, qA)
	if _, err := b.a.ExitEnable(bgx, app.ExitSettings{}); err != nil {
		t.Fatalf("ExitEnable: %v", err)
	}
	card := b.nd.peerCard(t)
	if card.ExitOffer() == nil {
		t.Fatal("the card of the running exit carries no exit offer")
	}
	// Creating the client registers exit/2 among the services this runtime offers, without dialing.
	cl, err := a.nd.rt.ExitClient(bgx, card, node.DialOpt{})
	if err != nil {
		t.Fatalf("ExitClient: %v", err)
	}
	_ = cl.Close()

	st := rawExitStream(t, a, b, card)
	if err := st.WriteFrame(exit.TypeUDPOpen, 0, exit.UDPOpen{Req: 7}.Append(nil)); err != nil {
		t.Fatalf("UDP_OPEN: %v", err)
	}
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := st.ReadFrame()
	if err != nil {
		t.Fatalf("UDP_OPEN reply: %v", err)
	}
	u, err := exit.ParseUDPOK(f.Payload)
	if f.Type != exit.TypeUDPOK || err != nil || u.Req != 7 || u.Code != exit.ResultDenied {
		t.Fatalf("UDP_OPEN reply %#x %+v %v, want DENIED for request 7", f.Type, u, err)
	}

	for _, typ := range []uint64{exit.TypeAccountGet, exit.TypeVoucher} {
		st := rawExitStream(t, a, b, card)
		if err := st.WriteFrame(typ, 0, nil); err != nil {
			t.Fatalf("frame %#x: %v", typ, err)
		}
		_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err := st.ReadFrame()
		var ce *mux.CloseError
		if !errors.As(err, &ce) || ce.Code != exit.CodePolicy {
			t.Fatalf("frame %#x ended with %v, want a policy close", typ, err)
		}
	}
}
