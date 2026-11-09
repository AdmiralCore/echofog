package tun_test

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/tun"
)

func TestCheckDNS(t *testing.T) {
	good := tun.DNSConfig{Tunnel: "EchoFog", Resolver: netip.MustParseAddr("198.18.0.2"), DisableSmartNameResolution: true}
	if err := tun.CheckDNS(good); err != nil {
		t.Fatalf("the resolver of the tunnel: %v", err)
	}
	v6 := good
	v6.Resolver = netip.MustParseAddr("fdfe:dcba:9876::2")
	if err := tun.CheckDNS(v6); err != nil {
		t.Fatalf("an IPv6 resolver inside the pool: %v", err)
	}
	for name, mut := range map[string]func(c *tun.DNSConfig){
		"none":              func(c *tun.DNSConfig) { c.Resolver = netip.Addr{} },
		"unspecified":       func(c *tun.DNSConfig) { c.Resolver = netip.IPv4Unspecified() },
		"public":            func(c *tun.DNSConfig) { c.Resolver = netip.MustParseAddr("8.8.8.8") },
		"LAN":               func(c *tun.DNSConfig) { c.Resolver = netip.MustParseAddr("192.168.1.1") },
		"loopback":          func(c *tun.DNSConfig) { c.Resolver = netip.MustParseAddr("127.0.0.1") },
		"next to the pool":  func(c *tun.DNSConfig) { c.Resolver = netip.MustParseAddr("198.20.0.1") },
		"ULA outside":       func(c *tun.DNSConfig) { c.Resolver = netip.MustParseAddr("fd00::2") },
		"zone":              func(c *tun.DNSConfig) { c.Resolver = netip.MustParseAddr("fe80::1%eth0") },
		"zone with a quote": func(c *tun.DNSConfig) { c.Resolver = netip.MustParseAddr("fdfe:dcba:9876::2%a';calc.exe;'") },
		"zone on the pool":  func(c *tun.DNSConfig) { c.Resolver = netip.MustParseAddr("fdfe:dcba:9876::2%x") },
		"v4-mapped public":  func(c *tun.DNSConfig) { c.Resolver = netip.MustParseAddr("::ffff:8.8.8.8") },
		"policy off":        func(c *tun.DNSConfig) { c.DisableSmartNameResolution = false },
	} {
		c := good
		mut(&c)
		if err := tun.CheckDNS(c); !errors.Is(err, tun.ErrBadDNS) {
			t.Errorf("%s: %v, want ErrBadDNS", name, err)
		}
	}
}
