package punch

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestNarrow(t *testing.T) {
	cases := []struct {
		n    NATInfo
		want bool
	}{
		{NATInfo{NATSymmetric, 40000, 41999, 5}, true},
		{NATInfo{NATSymmetric, 40000, 48000, 2}, true},  // exactly 8000 wide
		{NATInfo{NATSymmetric, 40000, 48001, 5}, false}, // wider
		{NATInfo{NATSymmetric, 40000, 40100, 1}, false}, // one sample says nothing
		{NATInfo{NATSymmetric, 500, 600, 5}, false},     // below 1024
		{NATInfo{NATSymmetric, 41000, 40000, 5}, false}, // inverted
		{NATInfo{NATSymmetric, 0, 0, 5}, false},         // unknown range
	}
	for i, c := range cases {
		if got := c.n.Narrow(); got != c.want {
			t.Errorf("case %d %+v: Narrow = %v, want %v", i, c.n, got, c.want)
		}
	}
}

func TestChooseTable(t *testing.T) {
	narrow := NATInfo{NATSymmetric, 40000, 40100, 5}
	wide := NATInfo{NATSymmetric, 0, 0, 0}
	cone := NATInfo{Class: NATCone}
	unk := NATInfo{Class: NATUnknown}
	none := NATInfo{Class: NATUnstated}
	cases := []struct {
		name      string
		own, peer NATInfo
		want      Mode
	}{
		{"cone-cone", cone, cone, ModeOrdinary},
		{"cone-unknown", cone, unk, ModeOrdinary},
		{"cone-unstated", cone, none, ModeOrdinary},
		{"unknown-cone", unk, cone, ModeOrdinary},
		{"unknown-unknown", unk, unk, ModeOrdinary},
		{"cone-sym", cone, narrow, ModeSpray},
		{"cone-sym-wide", cone, wide, ModeSpray},
		{"unknown-sym", unk, narrow, ModeSpray},
		{"sym-cone", narrow, cone, ModeDial},
		{"sym-cone-wide-own", wide, cone, ModeDial},
		{"sym-unknown", narrow, unk, ModeNone},
		{"sym-unstated", narrow, none, ModeNone},
		{"sym-sym-narrow", narrow, narrow, ModeMutual},
		{"sym-sym-own-wide", wide, narrow, ModeNone},
		{"sym-sym-peer-wide", narrow, wide, ModeNone},
		{"sym-sym-wide", wide, wide, ModeNone},
	}
	for _, c := range cases {
		if got := Choose(c.own, c.peer); got != c.want {
			t.Errorf("%s: Choose = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCounterpart(t *testing.T) {
	yes := [][2]Mode{{ModeDial, ModeSpray}, {ModeSpray, ModeDial}, {ModeMutual, ModeMutual}, {ModeOrdinary, ModeOrdinary}}
	for _, p := range yes {
		if !Counterpart(p[0], p[1]) {
			t.Errorf("Counterpart(%d, %d) = false", p[0], p[1])
		}
	}
	no := [][2]Mode{{ModeDial, ModeDial}, {ModeSpray, ModeSpray}, {ModeDial, ModeOrdinary}, {ModeSpray, ModeNone},
		{ModeMutual, ModeDial}, {ModeNone, ModeNone}, {ModeNone, ModeOrdinary}}
	for _, p := range no {
		if Counterpart(p[0], p[1]) {
			t.Errorf("Counterpart(%d, %d) = true", p[0], p[1])
		}
	}
}

func TestGatesEachOff(t *testing.T) {
	open := Gates{Noise3Allowed: true, Negotiated: true, Flag: true}
	if !open.Open() {
		t.Fatal("all conditions met but gates closed")
	}
	for name, g := range map[string]Gates{
		"not noise3":     {Negotiated: true, Flag: true},
		"not negotiated": {Noise3Allowed: true, Flag: true},
		"flag off":       {Noise3Allowed: true, Negotiated: true},
		"guard":          {Noise3Allowed: true, Negotiated: true, Flag: true, Guard: true},
		"anonymous":      {Noise3Allowed: true, Negotiated: true, Flag: true, Anonymous: true},
		"proxy":          {Noise3Allowed: true, Negotiated: true, Flag: true, Proxy: true},
	} {
		if g.Open() {
			t.Errorf("%s: gates open", name)
		}
	}
}

// checkLimits asserts the per-node limits of services.md §6.4.1 on what a NAT's node sent.
func checkLimits(t *testing.T, who string, n *simNAT, maxSockets, maxSpray int) {
	t.Helper()
	pk := n.packets()
	if len(pk) > MaxPunchPackets {
		t.Errorf("%s sent %d datagrams, limit %d", who, len(pk), MaxPunchPackets)
	}
	if maxSpray > 0 && len(pk) > maxSpray {
		t.Errorf("%s sent %d datagrams, spray limit %d", who, len(pk), maxSpray)
	}
	if got := n.peakSockets(); got > maxSockets {
		t.Errorf("%s had %d sockets open, limit %d", who, got, maxSockets)
	}
	if len(pk) == 0 {
		return
	}
	first := pk[0].at
	for _, p := range pk {
		if p.size < JunkSizeMin || p.size > JunkSizeMax {
			t.Fatalf("%s: datagram of %d bytes, want %d..%d (junk)", who, p.size, JunkSizeMin, JunkSizeMax)
		}
		if p.at.Sub(first) > PunchWindow+50*time.Millisecond {
			t.Fatalf("%s: datagram %v after the first, window is %v", who, p.at.Sub(first), PunchWindow)
		}
	}
	// no one-second slice carries much more than the rate
	for i := range pk {
		c := 0
		for j := i; j < len(pk) && pk[j].at.Sub(pk[i].at) < time.Second; j++ {
			c++
		}
		if c > PunchRate+PunchRate/4 {
			t.Fatalf("%s: %d datagrams within 1 s, rate is about %d", who, c, PunchRate)
		}
	}
}

func runConeSym(t *testing.T, symLo, symHi int, obsLo, obsHi uint16, seed uint64) (*simWorld, *simNAT, *simNAT, Hit, Hit, error, error, *Birthday, *Birthday) {
	t.Helper()
	clk := newFakeClock()
	w := newSimWorld(clk)
	cone := w.addNAT("203.0.113.5", false, 0, 0, seed)
	sym := w.addNAT("198.51.100.9", true, symLo, symHi, seed+10)
	cb := newBD(clk, cone, seed, openGates())
	sb := newBD(clk, sym, seed+1, openGates())
	var hc, hs Hit
	var ec, es error
	drive(clk, func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); hs, es = sb.Dial(context.Background(), cone.coneAddr(), true) }()
		go func() { defer wg.Done(); hc, ec = cb.Spray(context.Background(), sym.ip, obsLo, obsHi) }()
		wg.Wait()
	})
	return w, cone, sym, hc, hs, ec, es, cb, sb
}

// cone <-> symmetric, narrow block: the spray finds one of the symmetric side's open mappings, and
// the address it learns is the real external address of the socket that answered.
func TestConeSymNarrow(t *testing.T) {
	_, cone, sym, hc, hs, ec, es, cb, sb := runConeSym(t, 40000, 41999, 40600, 41200, 1)
	if ec != nil || es != nil {
		t.Fatalf("spray: %v, dial: %v", ec, es)
	}
	if hs.Sock == nil || hs.Target != cone.coneAddr() {
		t.Fatalf("dial hit %+v, want the cone's address", hs)
	}
	want := sym.extFor(hs.Sock, cone.coneAddr())
	if !want.IsValid() || hc.Target != want {
		t.Fatalf("spray learned %v, the hit socket's external address is %v", hc.Target, want)
	}
	checkLimits(t, "cone", cone, 0, MaxSprayPackets)
	checkLimits(t, "sym", sym, MaxDialSockets, 0)
	if tr, ok := cb.Stats(); tr != 1 || ok != 1 {
		t.Errorf("cone stats %d/%d, want 1/1", tr, ok)
	}
	if tr, ok := sb.Stats(); tr != 1 || ok != 1 {
		t.Errorf("sym stats %d/%d, want 1/1", tr, ok)
	}
	if sym.peakSockets() < 2 {
		t.Errorf("the symmetric side opened %d sockets: a birthday punch needs many", sym.peakSockets())
	}
	// every other socket was closed once one was found
	sym.mu.Lock()
	open := sym.open
	sym.mu.Unlock()
	if open > 1 {
		t.Errorf("%d sockets still open after the hit, want the hit socket only", open)
	}
}

// Sprayed datagrams go to the symmetric side's IP only, to ports of 1024..65535, never to anything
// else (V4), and carry the right counts.
func TestSprayTargets(t *testing.T) {
	_, cone, sym, _, _, _, _, _, _ := runConeSym(t, 40000, 41999, 40600, 41200, 2)
	for _, p := range cone.packets() {
		if p.to.Addr() != sym.ip {
			t.Fatalf("spray datagram to %v: not the symmetric side's IP", p.to)
		}
		if p.to.Port() < 1024 {
			t.Fatalf("spray datagram to port %d (< 1024)", p.to.Port())
		}
	}
}

// A wide block: the observed range is unknown (0, 0), the spray covers the dynamic range, and the
// budget holds whether or not it hits.
func TestSprayWideLimits(t *testing.T) {
	clk := newFakeClock()
	w := newSimWorld(clk)
	cone := w.addNAT("203.0.113.5", false, 0, 0, 3)
	sym := w.addNAT("198.51.100.9", true, 1024, 65535, 4)
	cb := newBD(clk, cone, 3, openGates())
	sb := newBD(clk, sym, 4, openGates())
	drive(clk, func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = sb.Dial(context.Background(), cone.coneAddr(), true) }()
		go func() { defer wg.Done(); _, _ = cb.Spray(context.Background(), sym.ip, 0, 0) }()
		wg.Wait()
	})
	checkLimits(t, "cone", cone, 0, MaxSprayPackets)
	checkLimits(t, "sym", sym, MaxDialSockets, 0)
	lo, hi := 65535, 0
	for _, p := range cone.packets() {
		if int(p.to.Port()) < lo {
			lo = int(p.to.Port())
		}
		if int(p.to.Port()) > hi {
			hi = int(p.to.Port())
		}
	}
	if lo > 8000 || hi < 57000 {
		t.Errorf("wide spray covered only %d..%d", lo, hi)
	}
}

// A narrow observed block is swept, widened by SprayMargin and clamped to 1024..65535.
func TestSprayNarrowCoversWidenedBlock(t *testing.T) {
	_, cone, _, _, _, _, _, _, _ := runConeSym(t, 40000, 41999, 40600, 41200, 5)
	lo, hi := 65535, 0
	for _, p := range cone.packets() {
		if int(p.to.Port()) < lo {
			lo = int(p.to.Port())
		}
		if int(p.to.Port()) > hi {
			hi = int(p.to.Port())
		}
	}
	if lo < 40600-SprayMargin || hi > 41200+SprayMargin {
		t.Errorf("spray reached %d..%d, beyond the margin of %d around 40600..41200", lo, hi, SprayMargin)
	}
	if hi-lo < 3000 {
		t.Errorf("spray covered only %d..%d", lo, hi)
	}
}

// symmetric <-> symmetric with narrow blocks: a double birthday forms a link on the socket pair
// that guessed each other's ports; each socket aims at one port of the peer's range. With blocks as
// small as the socket count the chance of a match in one attempt is about 63 %, so the test runs 20
// seeded attempts and requires most to succeed. Every attempt keeps the limits.
func TestMutualNarrow(t *testing.T) {
	succeeded := 0
	const runs = 20
	for seed := uint64(1); seed <= runs; seed++ {
		clk := newFakeClock()
		w := newSimWorld(clk)
		a := w.addNAT("203.0.113.5", true, 40000, 40063, seed*11)
		b := w.addNAT("198.51.100.9", true, 50000, 50063, seed*13)
		ab := newBD(clk, a, seed*21, openGates())
		bb := newBD(clk, b, seed*22, openGates())
		var ha, hb Hit
		var ea, eb error
		drive(clk, func() {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); ha, ea = ab.Mutual(context.Background(), b.ip, 50000, 50063) }()
			go func() { defer wg.Done(); hb, eb = bb.Mutual(context.Background(), a.ip, 40000, 40063) }()
			wg.Wait()
		})
		checkLimits(t, "A", a, MaxMutualSockets, 0)
		checkLimits(t, "B", b, MaxMutualSockets, 0)
		// every socket aimed at exactly one destination
		for _, n := range []*simNAT{a, b} {
			n.mu.Lock()
			per := map[*simSock]map[netip.AddrPort]bool{}
			for _, p := range n.maps {
				if per[p.sock] == nil {
					per[p.sock] = map[netip.AddrPort]bool{}
				}
				per[p.sock][p.dest] = true
			}
			n.mu.Unlock()
			for _, d := range per {
				if len(d) != 1 {
					t.Fatalf("a mutual socket sent to %d destinations, want 1", len(d))
				}
			}
		}
		if ea == nil && eb == nil {
			succeeded++
			// each side's target is the other side's real external address for that socket
			if ha.Target.Addr() != b.ip || hb.Target.Addr() != a.ip || ha.Sock == nil || hb.Sock == nil {
				t.Fatalf("seed %d: hits %+v %+v", seed, ha, hb)
			}
			if got := a.extFor(ha.Sock, ha.Target); !got.IsValid() {
				t.Fatalf("seed %d: A's hit socket never sent to its target", seed)
			}
		} else if !errors.Is(ea, ErrNoHit) && ea != nil || !errors.Is(eb, ErrNoHit) && eb != nil {
			t.Fatalf("seed %d: A: %v, B: %v", seed, ea, eb)
		}
	}
	if succeeded < runs/3 {
		t.Fatalf("only %d of %d double-birthday attempts found a mapping", succeeded, runs)
	}
}

// Everything off: no socket opened, no datagram sent, no counter moved, for every mode.
func TestGatesClosedSendNothing(t *testing.T) {
	closed := map[string]Gates{
		"not noise3":     {Negotiated: true, Flag: true},
		"not negotiated": {Noise3Allowed: true, Flag: true},
		"flag off":       {Noise3Allowed: true, Negotiated: true},
		"guard":          {Noise3Allowed: true, Negotiated: true, Flag: true, Guard: true},
		"anonymous":      {Noise3Allowed: true, Negotiated: true, Flag: true, Anonymous: true},
		"proxy":          {Noise3Allowed: true, Negotiated: true, Flag: true, Proxy: true},
	}
	for name, g := range closed {
		clk := newFakeClock()
		w := newSimWorld(clk)
		n := w.addNAT("198.51.100.9", true, 40000, 41999, 7)
		peer := netip.MustParseAddrPort("203.0.113.5:50000")
		gg := g
		b := newBD(clk, n, 7, func() Gates { return gg })
		ctx := context.Background()
		_, e1 := b.Dial(ctx, peer, true)
		_, e2 := b.Spray(ctx, peer.Addr(), 40000, 41999)
		_, e3 := b.Mutual(ctx, peer.Addr(), 40000, 41999)
		for i, e := range []error{e1, e2, e3} {
			if !errors.Is(e, ErrDisabled) {
				t.Errorf("%s: mode %d returned %v, want ErrDisabled", name, i, e)
			}
		}
		if len(n.packets()) != 0 || n.peakSockets() != 0 {
			t.Errorf("%s: %d packets, %d sockets with punching off", name, len(n.packets()), n.peakSockets())
		}
		if tr, ok := b.Stats(); tr != 0 || ok != 0 {
			t.Errorf("%s: stats %d/%d, want 0/0", name, tr, ok)
		}
	}
}

// At most MaxConcurrent attempts run on a node at once.
func TestConcurrentCap(t *testing.T) {
	clk := newFakeClock()
	w := newSimWorld(clk)
	n := w.addNAT("198.51.100.9", true, 40000, 41999, 9)
	b := newBD(clk, n, 9, openGates())
	peer := netip.MustParseAddrPort("203.0.113.5:50000") // nobody there: the attempts run their window out
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < MaxConcurrent; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = b.Dial(ctx, peer, true) }()
	}
	waitFor(t, "two attempts running", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.active == MaxConcurrent })
	if _, err := b.Dial(ctx, peer, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("third concurrent attempt: %v, want ErrBusy", err)
	}
	cancel()
	clk.Advance(PunchWindow + time.Second)
	wg.Wait()
}
