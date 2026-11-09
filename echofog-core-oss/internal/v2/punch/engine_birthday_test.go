package punch

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// bdRig is an engine with birthday punching over the simulated internet.
type bdRig struct {
	*rig
	w     *simWorld
	own   *simNAT
	bd    *Birthday
	gates Gates
	nat   NATInfo
}

func newBDRig(t *testing.T, role Role, ownSym bool, nat NATInfo, gates Gates) *bdRig {
	t.Helper()
	clk := newFakeClock()
	w := newSimWorld(clk)
	own := w.addNAT("198.51.100.9", ownSym, 40000, 41999, 77)
	r := &bdRig{w: w, own: own, gates: gates, nat: nat}
	rg := &rig{clk: clk, link: &fakeLink{rtt: 200 * time.Millisecond, clk: clk},
		net: &fakeNet{clk: clk, own: []netip.AddrPort{own.coneAddr()}}, bound: make(chan mux.Carrier, 4),
		rules: Rules{PeerIP: netip.MustParseAddr("203.0.113.5"), OwnIP: netip.MustParseAddr("198.51.100.9")}, allow: true}
	r.rig = rg
	r.bd = newBD(clk, own, 5, func() Gates { return r.gates })
	rg.eng = New(Config{
		Role: role, Link: rg.link, Net: rg.net, Clock: clk,
		Rand:      func(n int64) int64 { return n / 2 },
		Rules:     func() Rules { return rg.rules },
		Allowed:   func() bool { return true },
		HasDirect: func() bool { return false },
		Bind:      func(_ context.Context, c mux.Carrier) error { rg.bound <- c; return nil },
		Birthday:  r.bd,
		OwnNAT:    func() NATInfo { return r.nat },
		OwnID:     [32]byte{1},
		PeerID:    [32]byte{2},
	})
	return r
}

var openG = Gates{Noise3Allowed: true, Negotiated: true, Flag: true}

func (r *bdRig) answer(nonce uint64, o frame.PunchOffer) {
	o.Nonce = nonce
	r.eng.HandleControl(frame.Frame{Type: frame.TypePunchOffer, Payload: o.Append(nil)})
}

// The initiator's first offer states its NAT and observed range, and no mode (the answer comes
// later); with the gates closed it states none of it.
func TestOfferCarriesBirthdayFields(t *testing.T) {
	r := newBDRig(t, Initiator, true, NATInfo{NATSymmetric, 40000, 41999, 5}, openG)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.eng.Run(ctx) }()
	offerNonce(t, r.rig)
	o, _ := frame.ParsePunchOffer(r.link.frames(frame.TypePunchOffer)[0].payload)
	if o.Nat != uint32(NATSymmetric) || o.PortLo != 40000 || o.PortHi != 41999 || o.Samples != 5 || o.Mode != 0 {
		t.Fatalf("offer %+v", o)
	}
}

func TestOfferOmitsBirthdayFieldsWhenOff(t *testing.T) {
	for name, g := range map[string]Gates{
		"guard":     {Noise3Allowed: true, Negotiated: true, Flag: true, Guard: true},
		"anonymous": {Noise3Allowed: true, Negotiated: true, Flag: true, Anonymous: true},
		"proxy":     {Noise3Allowed: true, Negotiated: true, Flag: true, Proxy: true},
		"flag":      {Noise3Allowed: true, Negotiated: true},
		"noise3":    {Negotiated: true, Flag: true},
	} {
		r := newBDRig(t, Initiator, true, NATInfo{NATSymmetric, 40000, 41999, 5}, g)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = r.eng.Run(ctx) }()
		offerNonce(t, r.rig)
		o, _ := frame.ParsePunchOffer(r.link.frames(frame.TypePunchOffer)[0].payload)
		if o.Nat != 0 || o.PortLo != 0 || o.PortHi != 0 || o.Samples != 0 || o.Mode != 0 {
			t.Errorf("%s: offer %+v carries birthday fields", name, o)
		}
		cancel()
	}
}

// A symmetric initiator whose peer sent no NAT (an older build) cannot be answered by a spray:
// nothing at all goes out.
func TestSymInitiatorLegacyPeerSendsNothing(t *testing.T) {
	r := newBDRig(t, Initiator, true, NATInfo{NATSymmetric, 40000, 41999, 5}, openG)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.eng.Run(ctx) }()
	nonce := offerNonce(t, r.rig)
	r.answer(nonce, frame.PunchOffer{Addrs: []netip.AddrPort{netip.MustParseAddrPort("203.0.113.5:50000")}})
	for i := 0; i < 400; i++ {
		r.clk.Advance(50 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	if len(r.own.packets()) != 0 || r.own.peakSockets() != 0 || r.net.dialCount() != 0 {
		t.Fatalf("packets %d sockets %d dials %d towards a peer that cannot spray back",
			len(r.own.packets()), r.own.peakSockets(), r.net.dialCount())
	}
}

// A cone initiator, a symmetric peer answering with mode dial: the engine sprays the peer's range,
// learns the real external address of the socket that answered, and Inits it from the listener.
func TestConeInitiatorSpraysAndBinds(t *testing.T) {
	r := newBDRig(t, Initiator, false, NATInfo{Class: NATCone}, openG)
	symNAT := r.w.addNAT("203.0.113.5", true, 40000, 41999, 78)
	sym := newBD(r.clk, symNAT, 6, openGates())
	pair := r.net.result
	_ = pair
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.eng.Run(ctx) }()
	nonce := offerNonce(t, r.rig)
	r.answer(nonce, frame.PunchOffer{Addrs: []netip.AddrPort{netip.MustParseAddrPort("203.0.113.5:41000")},
		Nat: uint32(NATSymmetric), PortLo: 40600, PortHi: 41200, Samples: 4, Mode: uint32(ModeDial)})
	waitFor(t, "PUNCH_SYNC", func() bool { return len(r.link.frames(frame.TypePunchSync)) > 0 })
	// the peer starts its dial on the sync
	var hs Hit
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); hs, _ = sym.Dial(ctx, r.own.coneAddr(), true) }()
	advanceUntil(t, r.clk, 5*time.Millisecond, 4000, func() bool { return r.net.dialCount() > 0 })
	d := r.net.dials[0]
	// The Init arrives at that address: the socket behind it is the one the peer's dial must take
	// (any datagram from the cone's IP is a hit), whichever socket it had found by then.
	_ = r.own.listener.Send(d.targets[0], make([]byte, 40))
	done2 := make(chan struct{})
	go func() { wg.Wait(); close(done2) }()
	advanceUntil(t, r.clk, 5*time.Millisecond, 4000, func() bool {
		select {
		case <-done2:
			return true
		default:
			return false
		}
	})
	want := symNAT.extFor(hs.Sock, r.own.coneAddr())
	if len(d.targets) != 1 || d.targets[0] != want || !want.IsValid() {
		t.Fatalf("Init targets %v, want the answering socket's address %v", d.targets, want)
	}
	if n := len(r.net.junkSnapshot()); n != 0 {
		t.Fatalf("%d legacy junk datagrams sent in a spray attempt", n)
	}
	if got := len(r.own.packets()); got == 0 || got > MaxSprayPackets {
		t.Fatalf("spray sent %d datagrams", got)
	}
	if tr, ok := r.bd.Stats(); tr != 1 || ok != 1 {
		t.Fatalf("stats %d/%d", tr, ok)
	}
}

// stuckInitNet is the socket side of an Init that never completes: DialFrom waits for the deadline
// of the context it gets and remembers when that happened on the injected clock.
type stuckInitNet struct {
	simNet
	clk *fakeClock
	mu  sync.Mutex
	// starts and ends are the fake-clock times of each DialFrom call and its return.
	starts, ends []time.Time
}

func (b *stuckInitNet) DialFrom(ctx context.Context, _ Sock, _ netip.AddrPort, _ int) (mux.Carrier, error) {
	b.mu.Lock()
	b.starts = append(b.starts, b.clk.Now())
	b.mu.Unlock()
	<-ctx.Done()
	b.mu.Lock()
	b.ends = append(b.ends, b.clk.Now())
	b.mu.Unlock()
	return nil, ctx.Err()
}

func (b *stuckInitNet) firstInit() (calls int, took time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.starts) == 0 || len(b.ends) == 0 {
		return len(b.starts), 0
	}
	return len(b.starts), b.ends[0].Sub(b.starts[0])
}

func (b *stuckInitNet) initCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.starts)
}

func (b *stuckInitNet) initDone() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.ends)
}

// A false hit — a datagram with the peer's IP that no real flow stands behind — costs only an Init.
// That Init gets its own InitAfterHit from the moment of the hit, not the general AttemptTime of step
// 6; when it does not complete, the attempt fails and the session stays relayed. Without the separate
// deadline the Init borrows the whole attempt (5 s plus the 3 s punching window) and this goes red.
func TestDialInitAfterHitBoundedBySpec(t *testing.T) {
	r := newBDRig(t, Initiator, true, NATInfo{NATSymmetric, 40000, 41999, 5}, openG)
	peer := r.w.addNAT("203.0.113.5", false, 0, 0, 79)
	bn := &stuckInitNet{simNet: simNet{n: r.own}, clk: r.clk}
	r.bd = NewBirthday(BirthdayConfig{Clock: r.clk, Net: bn,
		Rand: func(n int64) int64 { return n / 2 }, Gates: func() Gates { return r.gates }})
	r.eng.cfg.Birthday = r.bd

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.eng.Run(ctx) }()
	nonce := offerNonce(t, r.rig)
	r.answer(nonce, frame.PunchOffer{Addrs: []netip.AddrPort{peer.coneAddr()},
		Nat: uint32(NATCone), Mode: uint32(ModeSpray)})
	advanceUntil(t, r.clk, 5*time.Millisecond, 4000, func() bool { return len(r.own.packets()) > 0 })
	spoofFromPeer(peer, r.own)
	advanceUntil(t, r.clk, 5*time.Millisecond, 4000, func() bool { return bn.initCalls() > 0 })
	advanceUntil(t, r.clk, 5*time.Millisecond, 4000, func() bool { return bn.initDone() > 0 })
	_, took := bn.firstInit()
	if took > InitAfterHit {
		t.Fatalf("Init after a false hit ran %v, the deadline is %v", took, InitAfterHit)
	}
	if took < InitAfterHit {
		t.Fatalf("Init after a false hit ended after %v, want the full %v", took, InitAfterHit)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after the attempt failed")
	}
}

// spoofFromPeer delivers a datagram from the peer's IP to every open birthday socket of n, as a
// packet with a forged source would arrive.
func spoofFromPeer(peer, n *simNAT) {
	n.mu.Lock()
	ports := make([]uint16, 0, len(n.maps))
	for _, m := range n.maps {
		if m.dest == peer.coneAddr() {
			ports = append(ports, m.ext)
		}
	}
	n.mu.Unlock()
	for _, p := range ports {
		_ = peer.listener.Send(netip.AddrPortFrom(n.ip, p), make([]byte, 40))
	}
}

// The responder side: a symmetric initiator's offer makes a cone responder spray on PUNCH_SYNC (and
// send no legacy junk), and a closed gate makes it silent.
func TestConeResponderSpraysOnSync(t *testing.T) {
	for _, closed := range []bool{false, true} {
		g := openG
		if closed {
			g.Guard = true
		}
		r := newBDRig(t, Responder, false, NATInfo{Class: NATCone}, g)
		off := frame.PunchOffer{Addrs: []netip.AddrPort{netip.MustParseAddrPort("203.0.113.5:41000")}, Nonce: 99,
			Nat: uint32(NATSymmetric), PortLo: 40600, PortHi: 41200, Samples: 4}
		r.eng.HandleControl(frame.Frame{Type: frame.TypePunchOffer, Payload: off.Append(nil)})
		ans, err := frame.ParsePunchOffer(r.link.frames(frame.TypePunchOffer)[0].payload)
		if err != nil {
			t.Fatal(err)
		}
		if !closed && (ans.Mode != uint32(ModeSpray) || ans.Nat != uint32(NATCone)) {
			t.Fatalf("answer %+v, want cone with mode spray", ans)
		}
		if closed && (ans.Mode != 0 || ans.Nat != 0) {
			t.Fatalf("answer %+v with the gate closed", ans)
		}
		r.eng.HandleControl(syncFrame(99))
		for i := 0; i < 700; i++ {
			r.clk.Advance(5 * time.Millisecond)
			time.Sleep(200 * time.Microsecond)
		}
		n := len(r.own.packets())
		if !closed && (n == 0 || n > MaxSprayPackets) {
			t.Errorf("open: spray sent %d datagrams", n)
		}
		if closed && n != 0 {
			t.Errorf("guard mode: %d datagrams", n)
		}
		r.eng.Close()
	}
}
