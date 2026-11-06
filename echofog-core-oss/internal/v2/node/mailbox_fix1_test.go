package node_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node"
)

// F dials no reserved address from a relay card (services.md 5.3): no private, loopback, link-local,
// shared, documentation, benchmarking or class E range, and no IPv6 form that embeds an IPv4 address
// (NAT64, 6to4, Teredo, IPv4-mapped), which could lead into the operator's own network.
func TestRelayCardAddressRule(t *testing.T) {
	bad := []string{
		"0.1.2.3", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.0.1", "192.0.0.5", "192.0.2.7",
		"192.88.99.1", "192.168.1.1", "198.18.0.1", "198.51.100.1", "203.0.113.9", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::", "::1", "fe80::1", "fc00::1", "ff02::1", "2001:db8::1", "64:ff9b::a00:1", "64:ff9b::808:808", "2002:0a00:0001::1",
		"2001:0:4136:e378:8000:63bf:3fff:fdd2", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "100::1",
	}
	for _, s := range bad {
		if node.RelayCardAddrDefault(netip.MustParseAddr(s)) {
			t.Errorf("%s was accepted", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "2a00:1450:4001:81b::200e"} {
		if !node.RelayCardAddrDefault(netip.MustParseAddr(s)) {
			t.Errorf("%s was refused", s)
		}
	}
}

// The fetch loop slows down on failures and does not hammer a relay that is down.
func TestFetchPause(t *testing.T) {
	p := time.Duration(0)
	var got []time.Duration
	for i := 0; i < 12; i++ {
		p = node.NextFetchPause(p, true, false)
		got = append(got, p)
	}
	want := []time.Duration{2, 4, 8, 16, 32, 64, 128, 256, 300, 300, 300, 300}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("pause after failure %d: %v, want %v", i+1, got[i], want[i]*time.Second)
		}
	}
	if p = node.NextFetchPause(p, false, false); p != time.Second {
		t.Fatalf("after a success: %v", p)
	}
	if p = node.NextFetchPause(time.Second, false, true); p != 5*time.Minute {
		t.Fatalf("in limited mode: %v", p)
	}
}
