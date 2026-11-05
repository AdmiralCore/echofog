package session_test

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/session"
)

// services.md §1: every limit is per session, the same limit applies per network diversity unit
// (IPv4 /24, IPv6 /32) times 3, and, where stated, per authenticated node ID per day, so a new
// session does not reset a quota.
func TestQuotaPerSession(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	rule := session.Rule{Name: "deliver", PerSession: 5, Window: time.Second}
	s1, s2 := [16]byte{1}, [16]byte{2}
	ip1, ip2 := addr("198.51.100.1"), addr("203.0.113.1")
	for i := 0; i < 5; i++ {
		if !l.Allow(rule, s1, ip1, [32]byte{1}) {
			t.Fatalf("call %d refused within the session limit", i)
		}
	}
	if l.Allow(rule, s1, ip1, [32]byte{1}) {
		t.Fatal("6th call in the window allowed")
	}
	if !l.Allow(rule, s2, ip2, [32]byte{2}) {
		t.Fatal("another session shares the quota")
	}
	clk.advance(1100 * time.Millisecond)
	if !l.Allow(rule, s1, ip1, [32]byte{1}) {
		t.Fatal("the window did not reset")
	}
}

func TestQuotaPerDiversityUnitFactorThree(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	rule := session.Rule{Name: "deliver", PerSession: 5, Window: time.Second}
	// Ten sessions from one /24 (different /32s, node IDs): together at most 3 x 5.
	allowed := 0
	for i := 0; i < 10; i++ {
		sid := [16]byte{byte(i + 1)}
		ip := addr("198.51.100.1")
		if i%2 == 1 {
			ip = addr("198.51.100.200")
		}
		for j := 0; j < 5; j++ {
			if l.Allow(rule, sid, ip, [32]byte{byte(i + 1)}) {
				allowed++
			}
		}
	}
	if allowed != 15 {
		t.Fatalf("allowed %d from one /24, want 15 (3 x per-session)", allowed)
	}
	// Another /24 is unaffected.
	if !l.Allow(rule, [16]byte{99}, addr("203.0.113.1"), [32]byte{99}) {
		t.Fatal("another /24 shares the unit quota")
	}
	// IPv6 unit is a /32.
	v6 := 0
	for i := 0; i < 10; i++ {
		ip := addr("2001:db8:1::1")
		if i%2 == 1 {
			ip = addr("2001:db8:ffff::1") // same /32
		}
		for j := 0; j < 5; j++ {
			if l.Allow(rule, [16]byte{0x60, byte(i)}, ip, [32]byte{0x60, byte(i)}) {
				v6++
			}
		}
	}
	if v6 != 15 {
		t.Fatalf("allowed %d from one v6 /32, want 15", v6)
	}
}

func TestQuotaPerNodeIDPerDaySurvivesNewSessions(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	rule := session.Rule{Name: "deposit", PerSession: 1000, Window: time.Hour, PerNodeDay: 10}
	node := [32]byte{7}
	ok := 0
	for i := 0; i < 30; i++ { // a new session and a new address every time
		ip := addr("198.51.100.1")
		ip = netipAddrOf(ip, i)
		if l.Allow(rule, [16]byte{byte(i + 1)}, ip, node) {
			ok++
		}
	}
	if ok != 10 {
		t.Fatalf("a node ID got %d deposits in a day over new sessions, want 10", ok)
	}
	if !l.Allow(rule, [16]byte{200}, addr("192.0.2.1"), [32]byte{8}) {
		t.Fatal("another node ID shares the quota")
	}
	clk.advance(25 * time.Hour)
	if !l.Allow(rule, [16]byte{201}, addr("192.0.2.1"), node) {
		t.Fatal("the daily quota did not reset")
	}
}

func TestQuotaStateIsBounded(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	rule := session.Rule{Name: "x", PerSession: 1, Window: time.Second}
	for i := 0; i < 300000; i++ {
		l.Allow(rule, [16]byte{byte(i), byte(i >> 8), byte(i >> 16)}, netipAddrOf(addr("10.0.0.1"), i), [32]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		if i%1000 == 0 {
			clk.advance(2 * time.Second)
		}
	}
	if n := l.Tracked(); n > session.MaxTrackedSources {
		t.Fatalf("tracked keys = %d, want at most %d", n, session.MaxTrackedSources)
	}
}
