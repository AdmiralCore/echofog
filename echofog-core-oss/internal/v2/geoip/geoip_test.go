package geoip

import (
	"net/netip"
	"testing"
)

// TestCountryOfKnownAddresses pins the answer for public addresses of six countries that matter
// for suite selection (wire.md §1, D21): three per country, IPv4 and IPv6 mixed.
func TestCountryOfKnownAddresses(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"77.88.8.8", "RU"},
		{"77.88.8.1", "RU"},
		{"87.250.250.242", "RU"},

		{"178.22.122.100", "IR"},
		{"185.51.200.2", "IR"},
		{"217.218.155.155", "IR"},

		{"114.114.114.114", "CN"},
		{"223.5.5.5", "CN"},
		{"180.76.76.76", "CN"},

		{"2001:1c00:1::1", "NL"},
		{"145.220.14.1", "NL"},
		{"213.154.241.1", "NL"},

		{"46.182.19.48", "DE"},
		{"185.12.64.1", "DE"},
		{"217.237.150.188", "DE"},

		{"8.8.8.8", "US"},
		{"8.8.4.4", "US"},
		{"9.9.9.9", "US"},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			got, ok := CountryOf(netip.MustParseAddr(tc.addr))
			if !ok || got != tc.want {
				t.Fatalf("CountryOf(%s) = %q, %v; want %q, true", tc.addr, got, ok, tc.want)
			}
		})
	}

	t.Run("ipv4-mapped", func(t *testing.T) {
		got, ok := CountryOf(netip.MustParseAddr("::ffff:8.8.8.8"))
		if !ok || got != "US" {
			t.Fatalf("CountryOf(::ffff:8.8.8.8) = %q, %v; want US, true", got, ok)
		}
	})

	// The zone is interface-local and must not prevent a lookup: it is stripped before both the
	// reserved check and the database search.
	t.Run("zoned ipv6", func(t *testing.T) {
		got, ok := CountryOf(netip.MustParseAddr("2606:4700:4700::1111%eth0"))
		if !ok || got != "US" {
			t.Fatalf("CountryOf(2606:4700:4700::1111%%eth0) = %q, %v; want US, true", got, ok)
		}
	})
}

// TestCountryOfUnknown pins that addresses that cannot name a country never do: invalid,
// private, loopback, link-local, multicast and other reserved space (wire.md §1 point 3:
// "an unknown country counts as a border").
func TestCountryOfUnknown(t *testing.T) {
	cases := []string{
		// IPv4 private, loopback, link-local and "this network"
		"0.0.0.0",
		"10.0.0.1",
		"127.0.0.1",
		"169.254.1.1",
		"172.16.0.1",
		"192.168.1.1",
		"100.64.0.1",
		// IPv4 reserved and documentation
		"192.0.0.1",
		"192.0.2.1",
		"198.18.0.1",
		"198.51.100.1",
		"203.0.113.1",
		"224.0.0.1",
		"240.0.0.1",
		"255.255.255.255",
		// IPv6 unspecified, loopback, unique-local, link-local, multicast, documentation
		"::",
		"::1",
		"fc00::1",
		"fd12:3456::1",
		"fe00::1",      // RFC 4291 reserved (fe00::/9)
		"fe7f:ffff::1", // last /9 block before fe80::/10
		"fe80::1",
		"ff02::1",
		"2001:db8::1",
		"2001::1",
		"2002::1",
		"100::1",
		"3fff::1",
		// Zoned IPv6 addresses must not slip past the reserved check.
		"2001::1%eth0",
		"3fff::1%eth0",
		"ff02::1%eth0",
		// IPv4-mapped private address
		"::ffff:192.168.1.1",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			a, err := netip.ParseAddr(s)
			if err != nil {
				t.Fatalf("ParseAddr(%q): %v", s, err)
			}
			if got, ok := CountryOf(a); ok {
				t.Fatalf("CountryOf(%s) = %q, true; want unknown", s, got)
			}
		})
	}

	t.Run("zero value", func(t *testing.T) {
		if got, ok := CountryOf(netip.Addr{}); ok {
			t.Fatalf("CountryOf(zero) = %q, true; want unknown", got)
		}
	})
}

// TestEmbeddedDataSize keeps the database inside the budget of T2.7: no more than 8 MiB of
// embedded data, so the mobile core stays small.
func TestEmbeddedDataSize(t *testing.T) {
	const budget = 8 << 20
	if len(dbBytes) == 0 {
		t.Fatal("no embedded database")
	}
	if len(dbBytes) > budget {
		t.Fatalf("embedded database is %d bytes, budget is %d", len(dbBytes), budget)
	}
}
