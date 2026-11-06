package tun_test

import (
	"errors"
	"math/rand/v2"
	"net/netip"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/tun"
)

var (
	udpEndpoint = netip.MustParseAddrPort("203.0.113.10:443")
	tcpEndpoint = netip.MustParseAddrPort("198.51.100.7:443")
	v6Endpoint  = netip.MustParseAddrPort("[2001:db8:1::5]:8443")
)

func testPlan() tun.Plan {
	return tun.Plan{
		Endpoints: []tun.Endpoint{
			{Addr: udpEndpoint, Proto: tun.ProtoUDP},
			{Addr: tcpEndpoint, Proto: tun.ProtoTCP},
			{Addr: v6Endpoint, Proto: tun.ProtoUDP},
		},
		TunnelPool: []netip.Prefix{netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("fdfe:dcba:9876::/48")},
	}
}

func mustRules(t testing.TB, p tun.Plan) []tun.Rule {
	t.Helper()
	r, err := tun.BuildRules(p)
	if err != nil {
		t.Fatalf("BuildRules: %v", err)
	}
	return r
}

func pkt(dir tun.Dir, ifc tun.Iface, proto tun.Proto, remote string) tun.Packet {
	return tun.Packet{Dir: dir, Iface: ifc, Proto: proto, Remote: netip.MustParseAddrPort(remote), Local: 40000}
}

func TestRulesArePersistentAndNamed(t *testing.T) {
	rules := mustRules(t, testPlan())
	if len(rules) < 4 {
		t.Fatalf("%d rules", len(rules))
	}
	names := map[string]bool{}
	for _, r := range rules {
		if !r.Persistent {
			t.Errorf("rule %q is not persistent: it would die with the helper", r.Name)
		}
		if r.Name == "" || names[r.Name] {
			t.Errorf("rule name %q is empty or repeated", r.Name)
		}
		names[r.Name] = true
	}
}

// Fail-closed: only the tunnel, loopback and the node's own endpoints get out.
func TestNothingLeavesOutsideTheTunnel(t *testing.T) {
	rules := mustRules(t, testPlan())
	blocked := []struct {
		name string
		p    tun.Packet
	}{
		{"public tcp", pkt(tun.Out, tun.IfacePhysical, tun.ProtoTCP, "93.184.216.34:443")},
		{"public udp (QUIC)", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "93.184.216.34:443")},
		{"icmp", pkt(tun.Out, tun.IfacePhysical, tun.ProtoICMP, "93.184.216.34:0")},
		{"IPv6 public tcp", pkt(tun.Out, tun.IfacePhysical, tun.ProtoTCP, "[2606:4700::1111]:443")},
		{"IPv6 public udp", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "[2606:4700::1111]:443")},
		{"IPv6 link-local", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "[fe80::1]:5353")},
		{"DNS udp to public resolver", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "8.8.8.8:53")},
		{"DNS tcp to public resolver", pkt(tun.Out, tun.IfacePhysical, tun.ProtoTCP, "8.8.8.8:53")},
		{"DNS to the router", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "192.168.1.1:53")},
		{"IPv6 DNS", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "[2001:4860:4860::8888]:53")},
		{"DNS over TLS", pkt(tun.Out, tun.IfacePhysical, tun.ProtoTCP, "1.1.1.1:853")},
		{"LAN without AllowLAN", pkt(tun.Out, tun.IfacePhysical, tun.ProtoTCP, "192.168.1.20:445")},
		{"inbound from the internet", pkt(tun.In, tun.IfacePhysical, tun.ProtoTCP, "93.184.216.34:443")},
		{"endpoint address, other port", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "203.0.113.10:444")},
		{"endpoint address, other protocol", pkt(tun.Out, tun.IfacePhysical, tun.ProtoTCP, "203.0.113.10:443")},
		{"endpoint port 53 (DNS beats the endpoint)", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "203.0.113.10:53")},
		{"DHCP without AllowDHCP", func() tun.Packet {
			p := pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "255.255.255.255:67")
			p.Local = 68
			return p
		}()},
	}
	for _, tc := range blocked {
		if got := tun.Evaluate(rules, tc.p); got != tun.Block {
			t.Errorf("%s: permitted", tc.name)
		}
	}
	permitted := []struct {
		name string
		p    tun.Packet
	}{
		{"loopback v4", pkt(tun.Out, tun.IfaceLoopback, tun.ProtoTCP, "127.0.0.1:1080")},
		{"loopback v6 in", pkt(tun.In, tun.IfaceLoopback, tun.ProtoTCP, "[::1]:1080")},
		{"tunnel v4", pkt(tun.Out, tun.IfaceTunnel, tun.ProtoTCP, "93.184.216.34:443")},
		{"tunnel v6", pkt(tun.Out, tun.IfaceTunnel, tun.ProtoUDP, "[2606:4700::1111]:443")},
		{"DNS inside the tunnel", pkt(tun.Out, tun.IfaceTunnel, tun.ProtoUDP, "198.18.0.2:53")},
		{"udp endpoint out", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "203.0.113.10:443")},
		{"udp endpoint reply", pkt(tun.In, tun.IfacePhysical, tun.ProtoUDP, "203.0.113.10:443")},
		{"tcp endpoint out", pkt(tun.Out, tun.IfacePhysical, tun.ProtoTCP, "198.51.100.7:443")},
		{"v6 endpoint out", pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "[2001:db8:1::5]:8443")},
	}
	for _, tc := range permitted {
		if got := tun.Evaluate(rules, tc.p); got != tun.Permit {
			t.Errorf("%s: blocked", tc.name)
		}
	}
}

func TestEvaluateDefaultsToBlock(t *testing.T) {
	if got := tun.Evaluate(nil, pkt(tun.Out, tun.IfacePhysical, tun.ProtoTCP, "1.2.3.4:5")); got != tun.Block {
		t.Fatal("no rules must mean block")
	}
}

func TestDHCPAndLANAreExplicit(t *testing.T) {
	p := testPlan()
	p.AllowDHCP, p.AllowLAN = true, true
	rules := mustRules(t, p)
	dhcp := pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "255.255.255.255:67")
	dhcp.Local = 68
	if tun.Evaluate(rules, dhcp) != tun.Permit {
		t.Error("DHCP blocked with AllowDHCP")
	}
	for _, ok := range []string{"192.168.1.20:445", "10.0.0.5:22", "[fd00::5]:445"} {
		if tun.Evaluate(rules, pkt(tun.Out, tun.IfacePhysical, tun.ProtoTCP, ok)) != tun.Permit {
			t.Errorf("LAN %s blocked with AllowLAN", ok)
		}
	}
	for _, bad := range []string{"93.184.216.34:443", "[2606:4700::1111]:443", "192.168.1.1:53", "[fd00::1]:53", "192.168.1.1:853"} {
		proto := tun.ProtoTCP
		if tun.Evaluate(rules, pkt(tun.Out, tun.IfacePhysical, proto, bad)) != tun.Block {
			t.Errorf("%s permitted with AllowLAN", bad)
		}
	}
	// DHCP is not a way to reach a public server.
	leak := pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "93.184.216.34:67")
	leak.Local = 68
	if tun.Evaluate(rules, leak) == tun.Permit {
		t.Error("DHCP rule permits a public server")
	}
}

func TestBadPlans(t *testing.T) {
	base := testPlan()
	with := func(e tun.Endpoint) tun.Plan {
		p := base
		p.Endpoints = append(append([]tun.Endpoint(nil), base.Endpoints...), e)
		return p
	}
	many := base
	many.Endpoints = nil
	for i := 0; i <= tun.MaxEndpoints; i++ {
		many.Endpoints = append(many.Endpoints, tun.Endpoint{
			Addr: netip.AddrPortFrom(netip.AddrFrom4([4]byte{203, 0, 113, byte(i + 1)}), 443), Proto: tun.ProtoUDP})
	}
	tests := map[string]tun.Plan{
		"unspecified":       with(tun.Endpoint{Addr: netip.MustParseAddrPort("0.0.0.0:443"), Proto: tun.ProtoUDP}),
		"loopback":          with(tun.Endpoint{Addr: netip.MustParseAddrPort("127.0.0.1:443"), Proto: tun.ProtoUDP}),
		"multicast":         with(tun.Endpoint{Addr: netip.MustParseAddrPort("224.0.0.251:443"), Proto: tun.ProtoUDP}),
		"broadcast":         with(tun.Endpoint{Addr: netip.MustParseAddrPort("255.255.255.255:443"), Proto: tun.ProtoUDP}),
		"inside the TUN":    with(tun.Endpoint{Addr: netip.MustParseAddrPort("198.18.0.2:443"), Proto: tun.ProtoUDP}),
		"v6 inside the TUN": with(tun.Endpoint{Addr: netip.MustParseAddrPort("[fdfe:dcba:9876::2]:443"), Proto: tun.ProtoUDP}),
		"port 0":            with(tun.Endpoint{Addr: netip.MustParseAddrPort("203.0.113.99:0"), Proto: tun.ProtoUDP}),
		"DNS port":          with(tun.Endpoint{Addr: netip.MustParseAddrPort("203.0.113.99:53"), Proto: tun.ProtoUDP}),
		"DoT port":          with(tun.Endpoint{Addr: netip.MustParseAddrPort("203.0.113.99:853"), Proto: tun.ProtoTCP}),
		"any protocol":      with(tun.Endpoint{Addr: netip.MustParseAddrPort("203.0.113.99:443"), Proto: tun.ProtoAny}),
		"icmp endpoint":     with(tun.Endpoint{Addr: netip.MustParseAddrPort("203.0.113.99:443"), Proto: tun.ProtoICMP}),
		"invalid address":   with(tun.Endpoint{Proto: tun.ProtoUDP}),
		"too many":          many,
	}
	for name, p := range tests {
		if _, err := tun.BuildRules(p); !errors.Is(err, tun.ErrBadPlan) {
			t.Errorf("%s: err = %v, want ErrBadPlan", name, err)
		}
	}
}

// A v4-mapped IPv6 endpoint is the IPv4 endpoint, not a way to open IPv6.
func TestMappedEndpointStaysV4(t *testing.T) {
	p := testPlan()
	p.Endpoints = []tun.Endpoint{{Addr: netip.MustParseAddrPort("[::ffff:203.0.113.77]:443"), Proto: tun.ProtoUDP}}
	rules := mustRules(t, p)
	if tun.Evaluate(rules, pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "203.0.113.77:443")) != tun.Permit {
		t.Error("the mapped endpoint does not permit its IPv4 address")
	}
	if tun.Evaluate(rules, pkt(tun.Out, tun.IfacePhysical, tun.ProtoUDP, "[2606:4700::1111]:443")) != tun.Block {
		t.Error("a mapped endpoint opened IPv6")
	}
}

// Whatever the plan, a packet on the physical interface is permitted only to an endpoint, to a
// LAN or DHCP address when asked, and never to a DNS port.
func checkInvariants(t testing.TB, p tun.Plan, rules []tun.Rule, rnd *rand.Rand) {
	t.Helper()
	for i := 0; i < 4000; i++ {
		var addr netip.Addr
		if rnd.IntN(2) == 0 {
			addr = netip.AddrFrom4([4]byte{byte(rnd.IntN(256)), byte(rnd.IntN(256)), byte(rnd.IntN(256)), byte(rnd.IntN(256))})
		} else {
			var b [16]byte
			for j := range b {
				b[j] = byte(rnd.IntN(256))
			}
			addr = netip.AddrFrom16(b)
		}
		port := []uint16{53, 853, 443, 80, 67, 547, 1, 65535, uint16(rnd.IntN(65536))}[rnd.IntN(9)]
		pk := tun.Packet{
			Dir:    tun.Dir(rnd.IntN(2)),
			Iface:  tun.IfacePhysical,
			Proto:  []tun.Proto{tun.ProtoTCP, tun.ProtoUDP, tun.ProtoICMP}[rnd.IntN(3)],
			Remote: netip.AddrPortFrom(addr, port),
			Local:  []uint16{68, 546, 40000}[rnd.IntN(3)],
		}
		if tun.Evaluate(rules, pk) != tun.Permit {
			continue
		}
		if port == 53 || port == 853 {
			t.Fatalf("DNS port permitted on the physical interface: %+v", pk)
		}
		a := addr.Unmap()
		isEndpoint := false
		for _, e := range p.Endpoints {
			if e.Addr.Addr().Unmap() == a && e.Addr.Port() == port && e.Proto == pk.Proto {
				isEndpoint = true
			}
		}
		lan := a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsLoopback()
		dhcp := (pk.Local == 68 || pk.Local == 546) && (port == 67 || port == 547)
		dhcpOK := a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast()
		if !isEndpoint && !(p.AllowLAN && lan) && !(p.AllowDHCP && dhcp && dhcpOK) {
			t.Fatalf("permitted without a reason: %+v (plan %+v)", pk, p)
		}
	}
}

func TestInvariantsRandomPackets(t *testing.T) {
	for _, allow := range []bool{false, true} {
		p := testPlan()
		p.AllowLAN, p.AllowDHCP = allow, allow
		checkInvariants(t, p, mustRules(t, p), rand.New(rand.NewPCG(1, uint64(len(p.Endpoints)))))
	}
}

func FuzzBuildRules(f *testing.F) {
	f.Add([]byte{203, 0, 113, 10}, uint16(443), uint8(2), true, true)
	f.Add([]byte{0x20, 0x01, 0xd, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, uint16(53), uint8(1), false, true)
	f.Fuzz(func(t *testing.T, ab []byte, port uint16, proto uint8, lan, dhcp bool) {
		addr, ok := netip.AddrFromSlice(ab)
		if !ok {
			return
		}
		p := testPlan()
		p.AllowLAN, p.AllowDHCP = lan, dhcp
		p.Endpoints = append(p.Endpoints, tun.Endpoint{Addr: netip.AddrPortFrom(addr, port), Proto: tun.Proto(proto % 4)})
		rules, err := tun.BuildRules(p)
		if err != nil {
			if !errors.Is(err, tun.ErrBadPlan) {
				t.Fatalf("error %v is not ErrBadPlan", err)
			}
			return
		}
		checkInvariants(t, p, rules, rand.New(rand.NewPCG(uint64(port), uint64(proto))))
	})
}
