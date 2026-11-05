package punch

import (
	"net/netip"
	"testing"
)

func aps(ss ...string) []netip.AddrPort {
	var out []netip.AddrPort
	for _, s := range ss {
		out = append(out, netip.MustParseAddrPort(s))
	}
	return out
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestFilterTargets(t *testing.T) {
	cases := []struct {
		name    string
		offered []netip.AddrPort
		rules   Rules
		want    []netip.AddrPort
	}{
		{"peer addr ip, any port", aps("203.0.113.5:1111", "203.0.113.5:2222"),
			Rules{PeerIP: ip("203.0.113.5"), OwnIP: ip("198.51.100.9")}, aps("203.0.113.5:1111", "203.0.113.5:2222")},
		{"card ip", aps("192.0.2.77:4433"),
			Rules{PeerIP: ip("203.0.113.5"), OwnIP: ip("198.51.100.9"), CardIPs: []netip.Addr{ip("192.0.2.77")}}, aps("192.0.2.77:4433")},
		{"foreign ip never", aps("8.8.8.8:53", "203.0.113.5:1"),
			Rules{PeerIP: ip("203.0.113.5"), OwnIP: ip("198.51.100.9")}, aps("203.0.113.5:1")},
		{"no PEER_ADDR and no card: nothing", aps("203.0.113.5:1"), Rules{}, nil},
		{"at most 4", aps("203.0.113.5:1", "203.0.113.5:2", "203.0.113.5:3", "203.0.113.5:4", "203.0.113.5:5"),
			Rules{PeerIP: ip("203.0.113.5")}, aps("203.0.113.5:1", "203.0.113.5:2", "203.0.113.5:3", "203.0.113.5:4")},
		{"duplicates count once", aps("203.0.113.5:1", "203.0.113.5:1", "203.0.113.5:2"),
			Rules{PeerIP: ip("203.0.113.5")}, aps("203.0.113.5:1", "203.0.113.5:2")},
		{"port 0 never", aps("203.0.113.5:0", "203.0.113.5:7"), Rules{PeerIP: ip("203.0.113.5")}, aps("203.0.113.5:7")},
		{"lan only when both PEER_ADDR ips are equal", aps("192.168.1.20:4433"),
			Rules{PeerIP: ip("203.0.113.5"), OwnIP: ip("198.51.100.9")}, nil},
		{"lan allowed behind one NAT", aps("192.168.1.20:4433", "10.0.0.7:1"),
			Rules{PeerIP: ip("203.0.113.5"), OwnIP: ip("203.0.113.5")}, aps("192.168.1.20:4433", "10.0.0.7:1")},
		{"lan in the card is still lan", aps("192.168.1.20:4433"),
			Rules{PeerIP: ip("203.0.113.5"), OwnIP: ip("198.51.100.9"), CardIPs: []netip.Addr{ip("192.168.1.20")}}, nil},
		{"loopback never, even behind one NAT", aps("127.0.0.1:4433", "[::1]:4433"),
			Rules{PeerIP: ip("203.0.113.5"), OwnIP: ip("203.0.113.5"), CardIPs: []netip.Addr{ip("127.0.0.1")}}, nil},
		{"link-local, multicast, unspecified never",
			aps("169.254.169.254:80", "224.0.0.1:1", "0.0.0.0:1", "[fe80::1]:1", "[ff02::1]:1"),
			Rules{PeerIP: ip("203.0.113.5"), OwnIP: ip("203.0.113.5")}, nil},
		{"v4-in-v6 is unmapped", aps("[::ffff:203.0.113.5]:9"), Rules{PeerIP: ip("203.0.113.5")}, aps("203.0.113.5:9")},
		{"ipv6 peer", aps("[2001:db8::5]:9", "[2001:db8::6]:9"),
			Rules{PeerIP: ip("2001:db8::5")}, aps("[2001:db8::5]:9")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FilterTargets(c.offered, c.rules)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}
