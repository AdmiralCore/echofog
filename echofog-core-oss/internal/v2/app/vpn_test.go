package app_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// ---- the VPN over an exit named by a link (T11.4) ------------------------------------------------------------

const exitURL = "echofog://join?v=2&card=EXIT"

// fakeExit stands for exit/2: it echoes what a tunnel is given, or refuses with a reason.
type fakeExit struct {
	mu     sync.Mutex
	err    error // what every dial answers
	dialed []string
	closed bool
}

func (f *fakeExit) DialExitStream(_ context.Context, target string) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	f.dialed = append(f.dialed, target)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(b, b); _ = b.Close() }()
	return a, nil
}

func (f *fakeExit) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeExit) setErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func (f *fakeExit) isClosed() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.closed }

type fakeDialer struct {
	link  *fakeExit
	cards []*objects.Card
	err   error
}

func (d *fakeDialer) Dial(_ context.Context, card *objects.Card) (app.ExitLink, error) {
	d.cards = append(d.cards, card)
	if d.err != nil {
		return nil, d.err
	}
	return d.link, nil
}

type vpnRig struct {
	*rig
	exit   *fakeExit
	dialer *fakeDialer
	clk    *tclock
}

func exitCard(t *testing.T, isExit bool) *objects.Card {
	t.Helper()
	body := &pb.NodeCard{}
	if isExit {
		body.Flags = objects.FlagExit
		body.Exit = &pb.ExitOffer{}
	}
	return &objects.Card{Body: body}
}

func newVPNRig(t *testing.T) *vpnRig {
	t.Helper()
	r := newRig(t, app.ModePublic)
	v := &vpnRig{rig: r, exit: &fakeExit{}, clk: &tclock{t: testNow}}
	v.dialer = &fakeDialer{link: v.exit}
	r.ln.preview[exitURL] = &links.Preview{Kind: links.KindContact, Node: nid(40), Device: nid(41), NotAfter: testNow.Add(time.Hour)}
	r.ln.peer = node.Peer{Node: node.NodeID(nid(40)), Device: node.DeviceID(nid(41)), Card: exitCard(t, true)}
	a, err := app.New(app.Config{Runtime: r.rt, Links: r.ln, Store: r.kv, Clock: v.clk, Self: nid(1), Mode: app.ModePublic, Version: "0.3.0",
		Exit: v.dialer, VPNListen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	r.a = a
	t.Cleanup(func() { _ = a.VPNStop() })
	return v
}

// socksConnect speaks SOCKS5 to the local proxy and asks for an IPv4 target; it returns the reply code
// and the connection.
func socksConnect(t *testing.T, listen, target string) (byte, net.Conn) {
	t.Helper()
	c, err := net.DialTimeout("tcp", listen, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
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

func TestVPNStartByLinkCarriesTraffic(t *testing.T) {
	v := newVPNRig(t)
	ctx := context.Background()
	st, err := v.a.VPNStart(ctx, exitURL)
	if err != nil {
		t.Fatalf("VPNStart: %v", err)
	}
	if !st.Running || st.Listen == "" || st.Exit != hx(nid(40)) {
		t.Fatalf("status %+v", st)
	}
	if len(v.ln.redeemed) != 1 || len(v.dialer.cards) != 1 || v.dialer.cards[0].ExitOffer() == nil {
		t.Fatalf("redeemed %v, dialed cards %d", v.ln.redeemed, len(v.dialer.cards))
	}
	if st.ExitState != "checking" {
		t.Fatalf("before the first tunnel the exit state is %q", st.ExitState)
	}
	code, c := socksConnect(t, st.Listen, "192.0.2.7:80")
	if code != 0 {
		t.Fatalf("SOCKS reply %d", code)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
	until(t, "the exit to be confirmed", func() bool { return v.a.VPNStatus().ExitState == "ok" })
	if got := v.a.VPNStatus(); got.Refused != 0 || !got.Running {
		t.Fatalf("status %+v", got)
	}
}

func TestVPNStartRefusals(t *testing.T) {
	v := newVPNRig(t)
	ctx := context.Background()
	if _, err := v.a.VPNStart(ctx, "not a link"); !errors.Is(err, links.ErrNotLink) {
		t.Fatalf("junk: %v", err)
	}
	v.ln.peer.Card = exitCard(t, false)
	if _, err := v.a.VPNStart(ctx, exitURL); !errors.Is(err, app.ErrNotExit) {
		t.Fatalf("not an exit: %v", err)
	}
	v.ln.peer.Card = nil
	if _, err := v.a.VPNStart(ctx, exitURL); !errors.Is(err, app.ErrNotExit) {
		t.Fatalf("no card: %v", err)
	}
	v.ln.peer.Card = exitCard(t, true)
	if v.a.VPNStatus().Running {
		t.Fatal("a refused start left the VPN running")
	}
	v.dialer.err = errors.New("no path")
	if _, err := v.a.VPNStart(ctx, exitURL); err == nil || v.a.VPNStatus().Running {
		t.Fatalf("dialer failure: %v", err)
	}
	v.dialer.err = nil
	if _, err := v.a.VPNStart(ctx, exitURL); err != nil {
		t.Fatal(err)
	}
	if _, err := v.a.VPNStart(ctx, exitURL); !errors.Is(err, app.ErrVPNRunning) {
		t.Fatalf("second start: %v", err)
	}
	if err := v.a.VPNStop(); err != nil {
		t.Fatal(err)
	}
	if v.a.VPNStatus().Running || !v.exit.isClosed() {
		t.Fatal("the VPN did not stop or the exit link stayed open")
	}
	if err := v.a.VPNStop(); err != nil {
		t.Fatalf("stopping a stopped VPN: %v", err)
	}
}

func TestVPNOff(t *testing.T) {
	r := newRig(t, app.ModePublic)
	if _, err := r.a.VPNStart(context.Background(), exitURL); !errors.Is(err, app.ErrVPNOff) {
		t.Fatalf("a VPN call without an exit dialer: %v", err)
	}
}

// The refusal reasons of T10.4 reach the status by their stable codes, and what they say about the
// exit is told apart from what they say about the site.
func TestVPNStatusCarriesTheReasonsOfRefusals(t *testing.T) {
	for _, tc := range []struct {
		reason    exit.Reason
		code      string
		exitState string // "" none
	}{
		{exit.ReasonPolicy, "policy", ""},
		{exit.ReasonBusy, "busy", ""},
		{exit.ReasonRateLimited, "rate-limited", ""},
		{exit.ReasonUnreachable, "unreachable", ""},
		{exit.ReasonNoPath, "no-path", "unreachable"},
		{exit.ReasonNoCamouflage, "no-camouflage", "no-camouflage"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			v := newVPNRig(t)
			st, err := v.a.VPNStart(context.Background(), exitURL)
			if err != nil {
				t.Fatal(err)
			}
			v.exit.setErr(&exit.Refusal{Reason: tc.reason})
			code, _ := socksConnect(t, st.Listen, "192.0.2.9:443")
			if code == 0 {
				t.Fatal("a refused tunnel was reported as open")
			}
			until(t, "the refusal to be counted", func() bool { return v.a.VPNStatus().Refused == 1 })
			got := v.a.VPNStatus()
			if got.LastReason != tc.code || got.RefusedBy[tc.code] != 1 || got.LastHost != "192.0.2.9:443" || got.LastAt == 0 {
				t.Fatalf("status %+v", got)
			}
			want := tc.exitState
			if want == "" {
				want = "checking"
			}
			if got.ExitState != want {
				t.Fatalf("exit state %q, want %q", got.ExitState, want)
			}
		})
	}
}

// A good tunnel after a bad one brings the exit state back to ok.
func TestVPNExitStateRecoversAfterATunnelThatWorks(t *testing.T) {
	v := newVPNRig(t)
	st, _ := v.a.VPNStart(context.Background(), exitURL)
	v.exit.setErr(&exit.Refusal{Reason: exit.ReasonNoPath})
	socksConnect(t, st.Listen, "192.0.2.9:443")
	until(t, "no-path", func() bool { return v.a.VPNStatus().ExitState == "unreachable" })
	v.exit.setErr(nil)
	if code, _ := socksConnect(t, st.Listen, "192.0.2.9:443"); code != 0 {
		t.Fatalf("reply %d", code)
	}
	until(t, "ok", func() bool { return v.a.VPNStatus().ExitState == "ok" })
}

func TestVPNSavedExitsAndHistory(t *testing.T) {
	v := newVPNRig(t)
	ctx := context.Background()
	if len(v.a.VPNExits()) != 0 {
		t.Fatal("exits before any was used")
	}
	st, err := v.a.VPNStart(ctx, exitURL)
	if err != nil {
		t.Fatal(err)
	}
	socksConnect(t, st.Listen, "192.0.2.7:80")
	until(t, "ok", func() bool { return v.a.VPNStatus().ExitState == "ok" })
	list := v.a.VPNExits()
	if len(list) != 1 || list[0].Node != hx(nid(40)) || !list[0].Active || list[0].Sessions != 1 || list[0].LastUsed == 0 {
		t.Fatalf("exits %+v", list)
	}
	if err := v.a.VPNName(hx(nid(40)), "Дом"); err != nil {
		t.Fatal(err)
	}
	if err := v.a.VPNFavorite(hx(nid(40)), true); err != nil {
		t.Fatal(err)
	}
	if got := v.a.VPNExits()[0]; got.Name != "Дом" || !got.Favorite {
		t.Fatalf("exit %+v", got)
	}
	if err := v.a.VPNName(hx(nid(99)), "x"); !errors.Is(err, app.ErrNoExit) {
		t.Fatalf("an exit that is not there: %v", err)
	}
	if err := v.a.VPNName(hx(nid(40)), strings.Repeat("я", 200)); !errors.Is(err, app.ErrAlias) {
		t.Fatalf("a name too long: %v", err)
	}
	if err := v.a.VPNStop(); err != nil {
		t.Fatal(err)
	}
	hist := v.a.VPNHistory()
	if len(hist) != 1 || hist[0].Node != hx(nid(40)) || hist[0].EndedAt == 0 || hist[0].EndReason != "stopped" {
		t.Fatalf("history %+v", hist)
	}
	// connect again by the saved exit: the link is redeemed again
	if _, err := v.a.VPNStartSaved(ctx, hx(nid(40))); err != nil {
		t.Fatalf("VPNStartSaved: %v", err)
	}
	if len(v.ln.redeemed) != 2 {
		t.Fatalf("redeemed %v", v.ln.redeemed)
	}
	_ = v.a.VPNStop()
	if err := v.a.VPNForget(hx(nid(40))); err != nil {
		t.Fatal(err)
	}
	if len(v.a.VPNExits()) != 0 {
		t.Fatal("a forgotten exit is still listed")
	}
	if _, err := v.a.VPNStartSaved(ctx, hx(nid(40))); !errors.Is(err, app.ErrNoExit) {
		t.Fatalf("a forgotten exit: %v", err)
	}
	if err := v.a.VPNClearHistory(); err != nil || len(v.a.VPNHistory()) != 0 {
		t.Fatalf("clear: %v %+v", err, v.a.VPNHistory())
	}
}

// The status is read while tunnels open and fail.
func TestVPNStatusUnderLoad(t *testing.T) {
	v := newVPNRig(t)
	st, _ := v.a.VPNStart(context.Background(), exitURL)
	v.exit.setErr(&exit.Refusal{Reason: exit.ReasonBusy})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = v.a.VPNStatus()
				c, err := net.DialTimeout("tcp", st.Listen, 2*time.Second)
				if err == nil {
					_ = c.Close()
				}
			}
		}()
	}
	wg.Wait()
	_ = v.a.VPNStatus()
}

type fakeRT struct {
	card *objects.Card
	err  error
}

func (f *fakeRT) ExitClient(_ context.Context, card *objects.Card, _ node.DialOpt) (*exit.Client, error) {
	f.card = card
	if f.err != nil {
		return nil, f.err
	}
	return exit.NewClient(exit.ClientConfig{OpenStream: func(context.Context) (*mux.Stream, error) { return nil, errors.New("no session") }})
}

func TestRuntimeExitDialsThroughTheRuntimeClient(t *testing.T) {
	rt := &fakeRT{}
	d := app.RuntimeExit(rt)
	card := exitCard(t, true)
	l, err := d.Dial(context.Background(), card)
	if err != nil || l == nil || rt.card != card {
		t.Fatalf("Dial: %v %v", l, err)
	}
	_ = l.Close()
	rt.err = errors.New("closed")
	if l, err := d.Dial(context.Background(), card); err == nil || l != nil {
		t.Fatalf("a failing runtime: %v %v", l, err)
	}
}

// Every refusal of the VPN reaches the screen as a neutral Russian text; the reasons of the exit speak
// with their own words, never with the target or the address.
func TestUserMessageSpeaksOfTheVPN(t *testing.T) {
	generic := app.UserMessage(errors.New("anything else"))
	for _, err := range []error{app.ErrNotExit, app.ErrVPNRunning, app.ErrVPNOff, app.ErrNoExit} {
		msg := app.UserMessage(err)
		if msg == generic || strings.Contains(msg, "app:") || !hasCyrillic(msg) {
			t.Errorf("%v: the screen says %q", err, msg)
		}
	}
	for _, r := range []exit.Reason{exit.ReasonNoCamouflage, exit.ReasonUnreachable, exit.ReasonBusy, exit.ReasonPolicy, exit.ReasonRateLimited, exit.ReasonNoPath} {
		err := &exit.Refusal{Reason: r}
		if r == exit.ReasonNoCamouflage { // has a screen of its own (T11.5): its text, still no address
			if got := app.UserMessage(fmt.Errorf("dial 203.0.113.9:443: %w", err)); got == generic || strings.Contains(got, "203.0.113.9") {
				t.Errorf("no-camouflage: %q", got)
			}
			continue
		}
		if got := app.UserMessage(err); got != r.Message() || got == generic {
			t.Errorf("%s: the screen says %q, want %q", r, got, r.Message())
		}
		if got := app.UserMessage(fmt.Errorf("dial 203.0.113.9:443: %w", err)); got != r.Message() || strings.Contains(got, "203.0.113.9") {
			t.Errorf("%s wrapped: %q", r, got)
		}
	}
}

type remoteRT struct {
	fakeRT
	ap    netip.AddrPort
	asked *objects.Card
}

func (r *remoteRT) ExitRemote(_ context.Context, card *objects.Card) (netip.AddrPort, error) {
	r.asked = card
	return r.ap, nil
}

// A runtime that knows the first hop of the session makes its links tell it (the whole-computer mode
// permits exactly that address); one that does not leaves plain links.
func TestRuntimeExitLinksTellTheirRemote(t *testing.T) {
	rt := &remoteRT{ap: netip.MustParseAddrPort("203.0.113.5:4433")}
	card := exitCard(t, true)
	l, err := app.RuntimeExit(rt).Dial(context.Background(), card)
	if err != nil {
		t.Fatal(err)
	}
	rl, ok := l.(app.RemoteLink)
	if !ok {
		t.Fatalf("%T is not a RemoteLink", l)
	}
	ap, err := rl.Remote(context.Background())
	if err != nil || ap != rt.ap || rt.asked != card {
		t.Fatalf("Remote = %v, %v (asked %v)", ap, err, rt.asked)
	}
	plain, err := app.RuntimeExit(&fakeRT{}).Dial(context.Background(), card)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.(app.RemoteLink); ok {
		t.Fatal("a link of a runtime that cannot tell its remote claims to")
	}
}
