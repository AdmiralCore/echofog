package noise3

import (
	"net/netip"
	"testing"
	"time"
)

// A-4: IPv6 sources are budgeted per /64, IPv4 per address.
func TestBudgetKeyIPv6Prefix(t *testing.T) {
	a := netip.MustParseAddr("2001:db8:1:2:aaaa::1")
	b := netip.MustParseAddr("2001:db8:1:2:bbbb::9")
	c := netip.MustParseAddr("2001:db8:1:3::1")
	if budgetKey(a) != budgetKey(b) {
		t.Fatal("two addresses of one /64 have different keys")
	}
	if budgetKey(a) == budgetKey(c) {
		t.Fatal("different /64 share a key")
	}
	if budgetKey(netip.MustParseAddr("192.0.2.1")) == budgetKey(netip.MustParseAddr("192.0.2.2")) {
		t.Fatal("IPv4 addresses share a key")
	}
	if budgetKey(netip.MustParseAddr("::ffff:192.0.2.1")) != budgetKey(netip.MustParseAddr("192.0.2.1")) {
		t.Fatal("v4-mapped address keyed differently from IPv4")
	}
}

func TestBudgetIPv6RotatingHostIDDoesNotMultiplyBudget(t *testing.T) {
	b := newBudgets()
	now := time.Unix(95_000, 0)
	allowed := 0
	for i := 0; i < 200; i++ {
		a := netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 1, 0, 2, 0, 0, 0, 0, 0, 0, byte(i >> 8), byte(i)})
		if b.allow(budgetKey(a), now) {
			allowed++
		}
	}
	if allowed != strangerPerIP {
		t.Fatalf("one /64 got %d packets, want %d", allowed, strangerPerIP)
	}
}
