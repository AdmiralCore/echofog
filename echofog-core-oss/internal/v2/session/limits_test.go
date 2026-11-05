package session_test

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/session"
)

func newLimits(t *testing.T, clk *obsClock, mut func(*session.LimitsConfig)) *session.Limits {
	t.Helper()
	cfg := session.LimitsConfig{Now: clk.Now}
	if mut != nil {
		mut(&cfg)
	}
	l, err := session.NewLimits(cfg)
	if err != nil {
		t.Fatalf("NewLimits: %v", err)
	}
	return l
}

func newClock() *obsClock { return &obsClock{t: time.Unix(1_800_000_000, 0)} }

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

// holdN admits n handshakes and keeps them open.
func holdN(l *session.Limits, n int, src func(i int) netip.Addr, hidden bool) []*session.Admission {
	out := make([]*session.Admission, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, l.Admit(src(i), hidden))
	}
	return out
}

func releaseAll(as []*session.Admission) {
	for _, a := range as {
		a.Release()
	}
}

// handshake.md §11: 64 per source IP, 256 per /24 (IPv6 /48), 1024 per node. A public node does
// not refuse above a limit: the puzzle gets 2 bits harder per exceeded limit.
func TestPublicOverLimitRaisesDifficultyByTwoBitsPerLimit(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	ip := addr("198.51.100.7")
	held := holdN(l, session.MaxPreAuthPerIP, func(int) netip.Addr { return ip }, false)
	for i, a := range held {
		if a.Refused() || a.Exceeded() != 0 {
			t.Fatalf("admission %d within the limit: refused=%v exceeded=%d", i, a.Refused(), a.Exceeded())
		}
	}
	over := l.Admit(ip, false)
	defer over.Release()
	if over.Refused() {
		t.Fatal("a public node must answer with a harder puzzle, not refuse")
	}
	if over.Exceeded() != 1 {
		t.Fatalf("exceeded = %d, want 1 (per-IP)", over.Exceeded())
	}
	base := l.Admit(addr("203.0.113.9"), false)
	defer base.Release()
	if got, want := over.Difficulty(), base.Difficulty()+2; got != want {
		t.Fatalf("difficulty over the per-IP limit = %d, want %d", got, want)
	}
	releaseAll(held)
	again := l.Admit(ip, false)
	defer again.Release()
	if again.Exceeded() != 0 {
		t.Fatalf("after release exceeded = %d, want 0", again.Exceeded())
	}
}

func TestPerUnitAndPerNodeLimits(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	// 256 handshakes from 256 distinct IPs of one /24: the /24 limit is reached, no IP limit is.
	held := holdN(l, session.MaxPreAuthPerUnit, func(i int) netip.Addr {
		return netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})
	}, false)
	over := l.Admit(addr("198.51.100.1"), false)
	if over.Exceeded() != 1 {
		t.Fatalf("exceeded = %d, want 1 (/24 only)", over.Exceeded())
	}
	over.Release()
	other := l.Admit(addr("198.51.101.1"), false)
	if other.Exceeded() != 0 {
		t.Fatalf("a neighbouring /24 inherited the limit: %d", other.Exceeded())
	}
	other.Release()
	releaseAll(held)

	// IPv6: the unit is a /48 (handshake §11).
	held = holdN(l, session.MaxPreAuthPerUnit, func(i int) netip.Addr {
		return netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(i)})
	}, false)
	over = l.Admit(addr("2001:db8:1::99"), false)
	if over.Exceeded() != 1 {
		t.Fatalf("v6 /48 limit: exceeded = %d, want 1", over.Exceeded())
	}
	over.Release()
	other = l.Admit(addr("2001:db8:2::1"), false)
	if other.Exceeded() != 0 {
		t.Fatalf("another /48 inherited the limit: %d", other.Exceeded())
	}
	other.Release()
	releaseAll(held)

	// 1024 per node, spread over 1024 different /24s.
	held = holdN(l, session.MaxPreAuthTotal, func(i int) netip.Addr {
		return netip.AddrFrom4([4]byte{10 + byte(i>>8), byte(i), 7, 1})
	}, false)
	over = l.Admit(addr("192.0.2.200"), false)
	if over.Exceeded() != 1 {
		t.Fatalf("exceeded = %d, want 1 (node total)", over.Exceeded())
	}
	over.Release()
	releaseAll(held)
}

// handshake §11: a hidden node queues the handshake and then closes silently; it never answers
// with a puzzle.
func TestHiddenOverLimitIsRefused(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	ip := addr("198.51.100.7")
	held := holdN(l, session.MaxPreAuthPerIP, func(int) netip.Addr { return ip }, true)
	over := l.Admit(ip, true)
	defer over.Release()
	if !over.Refused() {
		t.Fatal("a hidden node must refuse (uniform silent close) above a per-source limit")
	}
	releaseAll(held)
}

// handshake §11: "pre-authentication bans: none". However hard a source hammers, it is admitted
// again as soon as its load goes away.
func TestNoPreAuthBan(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	ip := addr("198.51.100.7")
	for i := 0; i < 5000; i++ {
		l.Admit(ip, true).Release()
	}
	clk.advance(2 * time.Second)
	a := l.Admit(ip, true)
	defer a.Release()
	if a.Refused() {
		t.Fatal("a source was refused after its load ended: that is a ban")
	}
	if a.Difficulty() != 0 {
		t.Fatalf("difficulty after the load ended = %d, want 0", a.Difficulty())
	}
}

// handshake §6: 0 below 25 handshakes/s, 16 below 100, 20 below 400, else 22; at most 24.
func TestDifficultyFollowsTheV1Table(t *testing.T) {
	cases := []struct {
		perSec int
		want   uint8
	}{{1, 0}, {23, 0}, {24, 16}, {99, 16}, {100, 20}, {399, 20}, {400, 22}, {3000, 22}}
	for _, tc := range cases {
		clk := newClock()
		l := newLimits(t, clk, nil)
		// Spread over distinct /24s so the per-unit cap does not hide the rate. The probe below
		// is the tc.perSec+1-th handshake of the second.
		for i := 0; i < tc.perSec; i++ {
			l.Admit(netip.AddrFrom4([4]byte{10 + byte(i>>8), byte(i), 9, 1}), false).Release()
		}
		a := l.Admit(addr("203.0.113.1"), false)
		if got := a.Difficulty(); got != tc.want {
			t.Errorf("%d/s: difficulty %d, want %d", tc.perSec, got, tc.want)
		}
		a.Release()
	}
}

func TestDifficultyNeverAbove24(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	ip := addr("198.51.100.7")
	var held []*session.Admission
	for i := 0; i < session.MaxPreAuthTotal; i++ {
		held = append(held, l.Admit(netip.AddrFrom4([4]byte{10 + byte(i>>8), byte(i), 9, 1}), false))
	}
	held = append(held, holdN(l, session.MaxPreAuthPerUnit, func(int) netip.Addr { return ip }, false)...)
	a := l.Admit(ip, false)
	defer a.Release()
	if a.Exceeded() < 3 {
		t.Fatalf("setup: exceeded = %d, want all three limits", a.Exceeded())
	}
	if a.Difficulty() > 24 {
		t.Fatalf("difficulty %d > 24 (an initiator refuses more)", a.Difficulty())
	}
	releaseAll(held)
}

// T3.5 "Готово": 10 000 HS_HELLO per second from one /24 must not hinder a handshake from
// another /24.
func TestFloodFromOneUnitDoesNotHinderAnother(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	for i := 0; i < 10000; i++ {
		l.Admit(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}), false).Release()
	}
	b := l.Admit(addr("203.0.113.5"), false)
	defer b.Release()
	if b.Refused() {
		t.Fatal("the other /24 was refused")
	}
	if b.Difficulty() != 0 {
		t.Fatalf("the other /24 pays difficulty %d, want 0", b.Difficulty())
	}
	a := l.Admit(addr("198.51.100.1"), false)
	defer a.Release()
	if a.Difficulty() < 20 {
		t.Fatalf("the flooding /24 pays only %d bits", a.Difficulty())
	}
}

// Many units together are real load: the puzzle turns on for everybody (handshake §6).
func TestLoadFromManyUnitsRaisesDifficultyForAll(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	for i := 0; i < 2000; i++ {
		l.Admit(netip.AddrFrom4([4]byte{10 + byte(i>>8), byte(i), 9, 1}), false).Release()
	}
	a := l.Admit(addr("203.0.113.5"), false)
	defer a.Release()
	if a.Difficulty() == 0 {
		t.Fatal("2000 handshakes/s from 2000 networks must switch the puzzle on")
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	ip := addr("198.51.100.7")
	a := l.Admit(ip, false)
	a.Release()
	a.Release()
	a.Release()
	held := holdN(l, session.MaxPreAuthPerIP, func(int) netip.Addr { return ip }, false)
	over := l.Admit(ip, false)
	if over.Exceeded() != 1 {
		t.Fatalf("exceeded = %d, want 1: the counter went wrong after a double release", over.Exceeded())
	}
	over.Release()
	releaseAll(held)
}

func TestAdmitIsRaceFree(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				a := l.Admit(netip.AddrFrom4([4]byte{198, 51, byte(g), byte(i)}), i%2 == 0)
				_ = a.Difficulty()
				a.Release()
			}
		}(g)
	}
	wg.Wait()
	a := l.Admit(addr("198.51.0.0"), false)
	defer a.Release()
	if a.Exceeded() != 0 {
		t.Fatalf("leaked counters: exceeded = %d", a.Exceeded())
	}
}

// Memory: the state of sources that went away is dropped.
func TestLimitsStateIsBounded(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	for i := 0; i < 200000; i++ {
		l.Admit(netip.AddrFrom4([4]byte{byte(i >> 16), byte(i >> 8), byte(i), 1}), false).Release()
		if i%1000 == 0 {
			clk.advance(time.Second)
		}
	}
	if n := l.Tracked(); n > session.MaxTrackedSources {
		t.Fatalf("tracked sources = %d, want at most %d", n, session.MaxTrackedSources)
	}
}
