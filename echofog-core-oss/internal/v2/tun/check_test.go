package tun_test

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/tun"
)

func TestCheckRulesAcceptsWhatBuildRulesMakes(t *testing.T) {
	for name, p := range map[string]tun.Plan{
		"plain":       testPlan(),
		"lan+dhcp":    func() tun.Plan { p := testPlan(); p.AllowLAN, p.AllowDHCP = true, true; return p }(),
		"no endpoint": {},
		"v4 only":     {Endpoints: []tun.Endpoint{{Addr: udpEndpoint, Proto: tun.ProtoUDP}}},
	} {
		if err := tun.CheckRules(mustRules(t, p)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckRulesRefusesAnOpenSet(t *testing.T) {
	good := mustRules(t, testPlan())
	with := func(extra ...tun.Rule) []tun.Rule {
		for i := range extra {
			extra[i].Persistent = true
			if extra[i].Name == "" {
				extra[i].Name = "extra"
			}
		}
		return append(append([]tun.Rule(nil), good...), extra...)
	}
	for name, rules := range map[string][]tun.Rule{
		"empty":          nil,
		"no block":       good[8:],
		"permit all out": with(tun.Rule{Name: "x1", Action: tun.Permit, Dir: tun.Out, Iface: tun.IfacePhysical, Weight: 100}),
		"permit all v6":  with(tun.Rule{Name: "x2", Action: tun.Permit, Dir: tun.Out, V6: true, Iface: tun.IfacePhysical, Weight: 100}),
		"permit all in":  with(tun.Rule{Name: "x3", Action: tun.Permit, Dir: tun.In, Iface: tun.IfaceAny, Weight: 100}),
		"permit a public /8": with(tun.Rule{Name: "x4", Action: tun.Permit, Dir: tun.Out, Iface: tun.IfacePhysical, Proto: tun.ProtoTCP,
			Remote: netip.MustParsePrefix("192.0.0.0/8"), RemotePort: 443, Weight: 100}),
		"permit dns above the block": with(tun.Rule{Name: "x5", Action: tun.Permit, Dir: tun.Out, Iface: tun.IfacePhysical, Proto: tun.ProtoUDP,
			RemotePort: 53, Weight: 250}),
		"permit icmp": with(tun.Rule{Name: "x6", Action: tun.Permit, Dir: tun.Out, Iface: tun.IfacePhysical, Proto: tun.ProtoICMP, Weight: 100}),
		"not persistent": func() []tun.Rule {
			r := append([]tun.Rule(nil), good...)
			r[3].Persistent = false
			return r
		}(),
		"unnamed": func() []tun.Rule {
			r := append([]tun.Rule(nil), good...)
			r[2].Name = ""
			return r
		}(),
		"repeated name": func() []tun.Rule {
			r := append([]tun.Rule(nil), good...)
			r[2].Name = r[1].Name
			return r
		}(),
		"too many": func() []tun.Rule {
			r := append([]tun.Rule(nil), good...)
			for i := 0; i < tun.MaxRules; i++ {
				r = append(r, tun.Rule{Name: "n" + string(rune('A'+i%26)) + string(rune('a'+i/26%26)) + string(rune('0'+i/676)), Action: tun.Block,
					Dir: tun.Out, Weight: 1, Persistent: true})
			}
			return r
		}(),
	} {
		err := tun.CheckRules(rules)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.Is(err, tun.ErrBadRules) {
			t.Errorf("%s: error %v is not ErrBadRules", name, err)
		}
	}
}

// D: each family of probe is held by a test: delete it from CheckRules and one of these goes green on
// a set that opens the Internet.
func TestCheckRulesProbeEveryFamily(t *testing.T) {
	good := mustRules(t, testPlan())
	add := func(r tun.Rule) []tun.Rule {
		r.Persistent, r.Name = true, "extra"
		return append(append([]tun.Rule(nil), good...), r)
	}
	for name, rules := range map[string][]tun.Rule{
		"a LAN resolver (TCP 53, above the DNS block)": add(tun.Rule{Action: tun.Permit, Dir: tun.Out, Iface: tun.IfacePhysical, Proto: tun.ProtoTCP,
			Remote: netip.MustParsePrefix("192.168.1.0/24"), RemotePort: 53, Weight: 250}),
		"a LAN resolver (DoT)": add(tun.Rule{Action: tun.Permit, Dir: tun.In, Iface: tun.IfacePhysical, Proto: tun.ProtoUDP,
			Remote: netip.MustParsePrefix("10.0.0.0/8"), RemotePort: 853, Weight: 250}),
		"a ULA resolver": add(tun.Rule{Action: tun.Permit, Dir: tun.Out, V6: true, Iface: tun.IfacePhysical, Proto: tun.ProtoUDP,
			Remote: netip.MustParsePrefix("fd00::/8"), RemotePort: 53, Weight: 250}),
		"a public IPv6 range (documentation)": add(tun.Rule{Action: tun.Permit, Dir: tun.Out, V6: true, Iface: tun.IfacePhysical, Proto: tun.ProtoTCP,
			Remote: netip.MustParsePrefix("2001:db8:ffff::/48"), RemotePort: 443, Weight: 100}),
		"a public IPv6 range (Cloudflare)": add(tun.Rule{Action: tun.Permit, Dir: tun.In, V6: true, Iface: tun.IfacePhysical, Proto: tun.ProtoUDP,
			Remote: netip.MustParsePrefix("2606:4700::/32"), Weight: 100}),
		"ICMPv6 echo to the world": add(tun.Rule{Action: tun.Permit, Dir: tun.Out, V6: true, Iface: tun.IfacePhysical, Proto: tun.ProtoICMP, Weight: 100}),
		"8.8.8.8": add(tun.Rule{Action: tun.Permit, Dir: tun.Out, Iface: tun.IfacePhysical, Proto: tun.ProtoUDP,
			Remote: netip.MustParsePrefix("8.8.8.8/32"), Weight: 100}),
	} {
		if err := tun.CheckRules(rules); !errors.Is(err, tun.ErrBadRules) {
			t.Errorf("%s: %v, want ErrBadRules", name, err)
		}
	}
}
