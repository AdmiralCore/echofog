package punch

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// --- fake clock (AD13) ---

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
	fired   bool
	c       *fakeClock
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f, c: c}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	rest := c.timers[:0]
	for _, t := range c.timers {
		switch {
		case t.stopped || t.fired:
		case !t.at.After(c.now):
			t.fired = true
			due = append(due, t)
		default:
			rest = append(rest, t)
		}
	}
	c.timers = rest
	c.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, t := range due {
		t.f()
	}
}

// advanceUntil steps the clock until cond holds; the real-time pause lets goroutines re-arm timers.
func advanceUntil(t *testing.T, c *fakeClock, step time.Duration, max int, cond func() bool) {
	t.Helper()
	for i := 0; i < max; i++ {
		if cond() {
			return
		}
		c.Advance(step)
		time.Sleep(3 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not reached by advancing the fake clock")
	}
}

// --- fakes for the session and the datagram side ---

type sentFrame struct {
	typ     uint64
	payload []byte
	at      time.Time
}

type fakeLink struct {
	mu      sync.Mutex
	sent    []sentFrame
	rtt     time.Duration
	clk     *fakeClock
	pingErr error
}

func (l *fakeLink) SendControl(typ uint64, _ byte, p []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, sentFrame{typ, append([]byte(nil), p...), l.clk.Now()})
	return nil
}

func (l *fakeLink) Ping(context.Context) (time.Duration, error) { return l.rtt, l.pingErr }

func (l *fakeLink) frames(typ uint64) []sentFrame {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []sentFrame
	for _, f := range l.sent {
		if f.typ == typ {
			out = append(out, f)
		}
	}
	return out
}

type junkPkt struct {
	to netip.AddrPort
	n  int
	at time.Time
}

type dialCall struct {
	targets []netip.AddrPort
	budget  int
	at      time.Time
}

type fakeNet struct {
	mu       sync.Mutex
	clk      *fakeClock
	own      []netip.AddrPort
	junk     []junkPkt
	dials    []dialCall
	result   mux.Carrier // returned by Dial when set
	failDial bool
}

func (n *fakeNet) OwnAddrs() []netip.AddrPort { return n.own }

func (n *fakeNet) SendJunk(to netip.AddrPort, p []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.junk = append(n.junk, junkPkt{to, len(p), n.clk.Now()})
	return nil
}

func (n *fakeNet) Dial(ctx context.Context, targets []netip.AddrPort, budget int) (mux.Carrier, error) {
	n.mu.Lock()
	n.dials = append(n.dials, dialCall{append([]netip.AddrPort(nil), targets...), budget, n.clk.Now()})
	res, fail := n.result, n.failDial
	n.mu.Unlock()
	if res != nil && !fail {
		return res, nil
	}
	// A dial that finds no flow runs until its 5 s attempt time ends.
	<-ctx.Done()
	return nil, ctx.Err()
}

func (n *fakeNet) dialCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.dials)
}

func (n *fakeNet) junkSnapshot() []junkPkt {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]junkPkt(nil), n.junk...)
}

type rig struct {
	clk    *fakeClock
	link   *fakeLink
	net    *fakeNet
	eng    *Engine
	bound  chan mux.Carrier
	rules  Rules
	allow  bool
	direct bool
	mu     sync.Mutex
}

func newRig(t *testing.T, role Role) *rig {
	t.Helper()
	clk := newFakeClock()
	r := &rig{clk: clk, link: &fakeLink{rtt: 200 * time.Millisecond, clk: clk},
		net: &fakeNet{clk: clk, own: []netip.AddrPort{netip.MustParseAddrPort("198.51.100.9:50000")}}, bound: make(chan mux.Carrier, 4),
		rules: Rules{PeerIP: netip.MustParseAddr("203.0.113.5"), OwnIP: netip.MustParseAddr("198.51.100.9")}, allow: true}
	r.eng = New(Config{
		Role: role, Link: r.link, Net: r.net, Clock: clk,
		Rand:      func(n int64) int64 { return n / 2 },
		Rules:     func() Rules { r.mu.Lock(); defer r.mu.Unlock(); return r.rules },
		Allowed:   func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.allow },
		HasDirect: func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.direct },
		Bind:      func(_ context.Context, c mux.Carrier) error { r.bound <- c; return nil },
	})
	return r
}

func offerFrame(nonce uint64, addrs ...string) frame.Frame {
	o := frame.PunchOffer{Nonce: nonce}
	for _, a := range addrs {
		o.Addrs = append(o.Addrs, netip.MustParseAddrPort(a))
	}
	return frame.Frame{Type: frame.TypePunchOffer, Payload: o.Append(nil)}
}

func syncFrame(nonce uint64) frame.Frame {
	return frame.Frame{Type: frame.TypePunchSync, Payload: binary.BigEndian.AppendUint64(nil, nonce)}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func offerNonce(t *testing.T, r *rig) uint64 {
	t.Helper()
	var nonce uint64
	waitFor(t, "PUNCH_OFFER", func() bool { return len(r.link.frames(frame.TypePunchOffer)) > 0 })
	o, err := frame.ParsePunchOffer(r.link.frames(frame.TypePunchOffer)[0].payload)
	if err != nil {
		t.Fatal(err)
	}
	nonce = o.Nonce
	if nonce == 0 {
		t.Fatal("zero nonce")
	}
	return nonce
}

// --- initiator ---

func TestInitiatorPunchesAndBinds(t *testing.T) {
	r := newRig(t, Initiator)
	pair := memcarrier.NewQUIC()
	r.net.result = pair.Client
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.eng.Run(ctx) }()

	nonce := offerNonce(t, r)
	// Our offer carries our own addresses.
	o, _ := frame.ParsePunchOffer(r.link.frames(frame.TypePunchOffer)[0].payload)
	if len(o.Addrs) != 1 || o.Addrs[0] != r.net.own[0] {
		t.Fatalf("offered %v, want %v", o.Addrs, r.net.own)
	}
	// B answers with the same nonce: one good target, one foreign IP, one loopback.
	r.eng.HandleControl(offerFrame(nonce, "203.0.113.5:61000", "8.8.8.8:53", "127.0.0.1:61000"))

	waitFor(t, "PUNCH_SYNC", func() bool { return len(r.link.frames(frame.TypePunchSync)) > 0 })
	sync1 := r.link.frames(frame.TypePunchSync)[0]
	if got := binary.BigEndian.Uint64(sync1.payload); got != nonce {
		t.Fatalf("PUNCH_SYNC nonce %x, want %x", got, nonce)
	}
	if r.net.dialCount() != 0 {
		t.Fatal("Init sent before RTT/2 passed")
	}
	// RTT is 200 ms: the Inits start 100 ms after PUNCH_SYNC.
	advanceUntil(t, r.clk, 10*time.Millisecond, 100, func() bool { return r.net.dialCount() > 0 })
	if d := r.net.dials[0].at.Sub(sync1.at); d < 100*time.Millisecond || d > 120*time.Millisecond {
		t.Fatalf("Init started %v after PUNCH_SYNC, want RTT/2 = 100ms", d)
	}
	if got := r.net.dials[0].targets; len(got) != 1 || got[0] != netip.MustParseAddrPort("203.0.113.5:61000") {
		t.Fatalf("targets %v: only the address with the PEER_ADDR IP is allowed", got)
	}
	if r.net.dials[0].budget != MaxInitDatagrams {
		t.Fatalf("budget %d, want %d", r.net.dials[0].budget, MaxInitDatagrams)
	}
	select {
	case c := <-r.bound:
		if c != pair.Client {
			t.Fatal("bound a different carrier")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("carrier not bound")
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestInitiatorWrongNonceNoSyncNoDial(t *testing.T) {
	r := newRig(t, Initiator)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.eng.Run(ctx) }()
	nonce := offerNonce(t, r)
	r.eng.HandleControl(offerFrame(nonce+1, "203.0.113.5:61000"))
	for i := 0; i < 20; i++ { // 10 s of fake time: the offer wait (5 s) ends with nothing sent
		r.clk.Advance(500 * time.Millisecond)
		time.Sleep(3 * time.Millisecond)
	}
	if n := len(r.link.frames(frame.TypePunchSync)); n != 0 || r.net.dialCount() != 0 {
		t.Fatalf("sync %d dials %d after an offer with the wrong nonce", n, r.net.dialCount())
	}
}

// No allowed target in the answer: nothing is sent to the network.
func TestInitiatorNoAllowedTargetNoDial(t *testing.T) {
	r := newRig(t, Initiator)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.eng.Run(ctx) }()
	nonce := offerNonce(t, r)
	r.eng.HandleControl(offerFrame(nonce, "8.8.8.8:53", "10.1.2.3:4433"))
	for i := 0; i < 20; i++ {
		r.clk.Advance(500 * time.Millisecond)
		time.Sleep(3 * time.Millisecond)
	}
	if len(r.link.frames(frame.TypePunchSync)) != 0 || r.net.dialCount() != 0 {
		t.Fatal("punch started without an allowed target")
	}
}

// Two attempts of 5 s, then the session stays relayed; the next round starts after uniform(90, 150)
// s, and PUNCH_OFFER is never sent twice within 30 s.
func TestInitiatorTwoAttemptsThenPauses(t *testing.T) {
	r := newRig(t, Initiator)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.eng.Run(ctx) }()
	nonce := offerNonce(t, r)
	r.eng.HandleControl(offerFrame(nonce, "203.0.113.5:61000"))
	advanceUntil(t, r.clk, 100*time.Millisecond, 400, func() bool { return r.net.dialCount() == 2 })
	if n := len(r.link.frames(frame.TypePunchSync)); n != 2 {
		t.Fatalf("PUNCH_SYNC sent %d times for 2 attempts", n)
	}
	d := r.net.dials
	if gap := d[1].at.Sub(d[0].at); gap < AttemptTime {
		t.Fatalf("second attempt after %v, the first lasts %v", gap, AttemptTime)
	}
	// The pause: no third dial and no new offer before 90 s.
	start := r.clk.Now()
	for r.clk.Now().Sub(start) < 85*time.Second {
		r.clk.Advance(time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	if r.net.dialCount() != 2 || len(r.link.frames(frame.TypePunchOffer)) != 1 {
		t.Fatalf("round resumed early: dials %d offers %d", r.net.dialCount(), len(r.link.frames(frame.TypePunchOffer)))
	}
	// Rand returns n/2: the pause is 90 s + 30 s = 120 s after the failed round.
	advanceUntil(t, r.clk, time.Second, 200, func() bool { return len(r.link.frames(frame.TypePunchOffer)) == 2 })
	offers := r.link.frames(frame.TypePunchOffer)
	if gap := offers[1].at.Sub(offers[0].at); gap < RetryMin || gap > RetryMax+AttemptTime*3 {
		t.Fatalf("next offer %v after the first, want within the retry window", gap)
	}
	if gap := offers[1].at.Sub(offers[0].at); gap < OfferInterval {
		t.Fatalf("offers %v apart, want at least %v", gap, OfferInterval)
	}
}

func TestInitiatorStopsWhenDirect(t *testing.T) {
	r := newRig(t, Initiator)
	r.mu.Lock()
	r.direct = true
	r.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.eng.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on a session that is already direct")
	}
	if len(r.link.frames(frame.TypePunchOffer)) != 0 {
		t.Fatal("offer sent on a direct session")
	}
}

func TestInitiatorNotAllowedStaysSilent(t *testing.T) {
	r := newRig(t, Initiator)
	r.mu.Lock()
	r.allow = false
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.eng.Run(ctx) }()
	for i := 0; i < 10; i++ {
		r.clk.Advance(time.Second)
		time.Sleep(3 * time.Millisecond)
	}
	cancel()
	if len(r.link.frames(frame.TypePunchOffer)) != 0 {
		t.Fatal("offer sent while punching is not allowed (other country / no feature)")
	}
}

func TestInitiatorRunEndsWithContext(t *testing.T) {
	r := newRig(t, Initiator)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.eng.Run(ctx) }()
	offerNonce(t, r)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run ignored cancellation")
	}
}

// --- responder ---

func TestResponderAnswersOfferAndJunksOnSync(t *testing.T) {
	r := newRig(t, Responder)
	r.eng.HandleControl(offerFrame(42, "203.0.113.5:61000", "203.0.113.5:61001", "8.8.8.8:53"))
	offers := r.link.frames(frame.TypePunchOffer)
	if len(offers) != 1 {
		t.Fatalf("answered with %d offers", len(offers))
	}
	o, err := frame.ParsePunchOffer(offers[0].payload)
	if err != nil || o.Nonce != 42 || len(o.Addrs) != 1 || o.Addrs[0] != r.net.own[0] {
		t.Fatalf("answer %+v %v", o, err)
	}
	if len(r.net.junkSnapshot()) != 0 {
		t.Fatal("junk before PUNCH_SYNC")
	}
	r.eng.HandleControl(syncFrame(42))
	advanceUntil(t, r.clk, 5*time.Millisecond, 400, func() bool { return len(r.net.junkSnapshot()) >= 2*JunkMin })
	// Let the junk finish.
	for i := 0; i < 60; i++ {
		r.clk.Advance(5 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	per := map[netip.AddrPort][]junkPkt{}
	for _, j := range r.net.junkSnapshot() {
		per[j.to] = append(per[j.to], j)
	}
	if len(per) != 2 {
		t.Fatalf("junk went to %d addresses, want the 2 allowed targets: %v", len(per), per)
	}
	for to, js := range per {
		if to.Addr() != netip.MustParseAddr("203.0.113.5") {
			t.Fatalf("junk to %v", to)
		}
		if len(js) < JunkMin || len(js) > JunkMax {
			t.Fatalf("%d junk datagrams to %v, want %d..%d", len(js), to, JunkMin, JunkMax)
		}
		for i, j := range js {
			if j.n < JunkSizeMin || j.n > JunkSizeMax {
				t.Fatalf("junk size %d", j.n)
			}
			if i > 0 {
				if gap := j.at.Sub(js[i-1].at); gap < JunkGapMin-time.Millisecond || gap > JunkGapMax+10*time.Millisecond {
					t.Fatalf("junk gap %v, want %v..%v", gap, JunkGapMin, JunkGapMax)
				}
			}
		}
	}
}

func TestResponderIgnoresSyncWithOtherNonce(t *testing.T) {
	r := newRig(t, Responder)
	r.eng.HandleControl(offerFrame(42, "203.0.113.5:61000"))
	r.eng.HandleControl(syncFrame(43))
	r.eng.HandleControl(syncFrame(0))
	for i := 0; i < 30; i++ {
		r.clk.Advance(10 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	if n := len(r.net.junkSnapshot()); n != 0 {
		t.Fatalf("%d junk datagrams for a PUNCH_SYNC that matches no offer", n)
	}
}

func TestResponderSyncWithoutOfferDoesNothing(t *testing.T) {
	r := newRig(t, Responder)
	r.eng.HandleControl(syncFrame(7))
	for i := 0; i < 30; i++ {
		r.clk.Advance(10 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	if len(r.net.junkSnapshot()) != 0 {
		t.Fatal("junk without an offer")
	}
}

func TestResponderOfferAtMostOncePer30s(t *testing.T) {
	r := newRig(t, Responder)
	r.eng.HandleControl(offerFrame(1, "203.0.113.5:61000"))
	r.clk.Advance(10 * time.Second)
	r.eng.HandleControl(offerFrame(2, "203.0.113.5:61000"))
	if n := len(r.link.frames(frame.TypePunchOffer)); n != 1 {
		t.Fatalf("answered %d offers within 30 s, want 1", n)
	}
	r.clk.Advance(21 * time.Second)
	r.eng.HandleControl(offerFrame(3, "203.0.113.5:61000"))
	if n := len(r.link.frames(frame.TypePunchOffer)); n != 2 {
		t.Fatalf("answered %d offers after 31 s, want 2", n)
	}
}

func TestResponderNotAllowedIsSilent(t *testing.T) {
	r := newRig(t, Responder)
	r.mu.Lock()
	r.allow = false
	r.mu.Unlock()
	r.eng.HandleControl(offerFrame(1, "203.0.113.5:61000"))
	r.eng.HandleControl(syncFrame(1))
	if len(r.link.frames(frame.TypePunchOffer)) != 0 || len(r.net.junkSnapshot()) != 0 {
		t.Fatal("responder acted while punching is not allowed")
	}
}

func TestResponderMalformedOfferIgnored(t *testing.T) {
	r := newRig(t, Responder)
	r.eng.HandleControl(frame.Frame{Type: frame.TypePunchOffer, Payload: []byte{0x0a, 3, 9, 1, 2}})
	r.eng.HandleControl(frame.Frame{Type: frame.TypePunchSync, Payload: []byte{1, 2, 3}})
	if len(r.link.frames(frame.TypePunchOffer)) != 0 {
		t.Fatal("answered a malformed offer")
	}
}

// An initiator engine never answers an offer it did not ask for (both ends offering would race).
func TestInitiatorDoesNotAnswerForeignOffer(t *testing.T) {
	r := newRig(t, Initiator)
	r.mu.Lock()
	r.direct = false
	r.mu.Unlock()
	r.eng.HandleControl(offerFrame(5, "203.0.113.5:61000"))
	if len(r.link.frames(frame.TypePunchOffer)) != 0 {
		t.Fatal("initiator answered an unsolicited offer")
	}
}

// F2: a peer that floods PUNCH frames must not make the node verify cards and look up countries for
// each one: Allowed is consulted only for a frame that passed the cheap checks (rate, pending nonce).
func TestResponderFloodDoesNotCallAllowed(t *testing.T) {
	r := newRig(t, Responder)
	var calls int
	var mu sync.Mutex
	r.eng = New(Config{Role: Responder, Link: r.link, Net: r.net, Clock: r.clk, Rand: func(n int64) int64 { return n / 2 },
		Rules:   func() Rules { return r.rules },
		Allowed: func() bool { mu.Lock(); calls++; mu.Unlock(); return true }})
	r.eng.HandleControl(offerFrame(1, "203.0.113.5:61000"))
	for i := 0; i < 200; i++ {
		r.eng.HandleControl(offerFrame(uint64(i+2), "203.0.113.5:61000")) // all within 30 s
		r.eng.HandleControl(syncFrame(uint64(1000 + i)))                  // no pending nonce
	}
	mu.Lock()
	defer mu.Unlock()
	if calls > 2 {
		t.Fatalf("Allowed called %d times for a flood of 400 frames, want at most 2", calls)
	}
}
