// Package tun is the policy side of the whole-computer VPN mode of v2 (T10.6): the firewall rules
// of the kill switch, the DNS protection and the state machine that keeps them fail-closed. It
// owns no device and no firewall. The platform drivers (Windows WFP with persistent filters,
// Linux nftables, the Wintun/TUN helper of internal/network/tunmode) sit behind the interfaces of
// driver.go, so everything here runs against fakes.
//
// The rules are data. BuildRules turns a Plan into them and Evaluate is a reference evaluator of
// the same data: the tests prove with it that nothing leaves the computer outside the tunnel.
package tun

import (
	"errors"
	"fmt"
	"net/netip"
)

// Action of a rule.
type Action uint8

const (
	Block Action = iota
	Permit
)

// Dir is the direction of a packet or a rule.
type Dir uint8

const (
	Out Dir = iota
	In
)

// Iface is where a packet is seen.
type Iface uint8

const (
	IfaceAny Iface = iota
	IfaceLoopback
	IfaceTunnel
	IfacePhysical
)

// Proto of a rule or packet.
type Proto uint8

const (
	ProtoAny Proto = iota
	ProtoTCP
	ProtoUDP
	ProtoICMP
)

// Rule is one filter. The highest Weight that matches decides; on a tie Block wins. Every rule is
// Persistent: the firewall keeps it when the process that installed it dies (that is what makes
// the kill switch survive a killed helper).
type Rule struct {
	Name       string
	Action     Action
	Dir        Dir
	Iface      Iface
	Proto      Proto
	Remote     netip.Prefix // zero: any address (of the rule's family)
	V6         bool         // the rule applies to IPv6 (otherwise IPv4)
	RemotePort uint16       // 0: any
	LocalPort  uint16       // 0: any
	// ICMPLo..ICMPHi limit an ICMP rule to these message types (both 0: any type).
	ICMPLo, ICMPHi uint8
	Weight         uint8
	Persistent     bool
}

// Packet is what Evaluate decides on.
type Packet struct {
	Dir    Dir
	Iface  Iface
	Proto  Proto
	Remote netip.AddrPort
	Local  uint16
	// ICMPType is the message type of an ICMP packet (ICMPv6: 133 router solicitation ... 136
	// neighbour advertisement).
	ICMPType uint8
}

// Plan describes what the kill switch must allow around the tunnel.
type Plan struct {
	// Endpoints are the addresses the node's own transport talks to outside the tunnel (the exit
	// and relays it connects to). Each is permitted for its protocol only.
	Endpoints []Endpoint
	// TunnelPool is the address range of the TUN interface (v1: 198.18.0.0/15) and its IPv6 range;
	// endpoints inside it are refused.
	TunnelPool []netip.Prefix
	// AllowLAN permits private and link-local ranges on the physical interface. Off by default.
	AllowLAN bool
	// AllowDHCP permits DHCPv4/v6 on the physical interface so the link can renew its lease.
	AllowDHCP bool
}

// Endpoint is one address:port the node reaches outside the tunnel.
type Endpoint struct {
	Addr  netip.AddrPort
	Proto Proto // ProtoTCP or ProtoUDP
}

// Limits of a Plan.
const MaxEndpoints = 64

var (
	// ErrBadPlan is a Plan that cannot be turned into safe rules.
	ErrBadPlan = errors.New("tun: bad kill switch plan")
)

// Weights: the default block is the lowest, permits sit above it, and the DNS block above every
// permit so that no permit, however it came about, opens a resolver outside the tunnel.
const (
	weightBlock  = 1
	weightPermit = 10
	weightEndpt  = 20
	weightDNS    = 30
)

// dnsPorts are the ports of plain DNS and DNS over TLS. On the physical interface they are
// always blocked: names are resolved inside the tunnel (the fake DNS of the node) or not at all.
var dnsPorts = []uint16{53, 853}

var (
	lanV4 = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("169.254.0.0/16"),
	}
	lanV6 = []netip.Prefix{netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("fc00::/7")}

	dhcpV4 = append([]netip.Prefix{netip.MustParsePrefix("255.255.255.255/32")}, lanV4...)
	dhcpV6 = []netip.Prefix{netip.MustParsePrefix("ff02::/16"), netip.MustParsePrefix("fe80::/10")}
)

// BuildRules turns a Plan into the persistent rule set of the kill switch: everything is blocked
// in both families and both directions; loopback and the tunnel are permitted; the node's own
// endpoints are permitted for their protocol and port; LAN and DHCP only when asked; DNS ports
// stay blocked outside the tunnel whatever else is permitted.
func BuildRules(p Plan) ([]Rule, error) {
	if len(p.Endpoints) > MaxEndpoints {
		return nil, fmt.Errorf("%w: %d endpoints, at most %d", ErrBadPlan, len(p.Endpoints), MaxEndpoints)
	}
	for _, pool := range p.TunnelPool {
		if !pool.IsValid() {
			return nil, fmt.Errorf("%w: invalid tunnel pool", ErrBadPlan)
		}
	}
	var rules []Rule
	add := func(r Rule) {
		r.Persistent = true
		rules = append(rules, r)
	}
	both := func(f func(dir Dir, v6 bool)) {
		for _, dir := range []Dir{Out, In} {
			for _, v6 := range []bool{false, true} {
				f(dir, v6)
			}
		}
	}
	fam := func(v6 bool) string {
		if v6 {
			return "v6"
		}
		return "v4"
	}
	dirName := func(d Dir) string {
		if d == In {
			return "in"
		}
		return "out"
	}

	both(func(d Dir, v6 bool) {
		add(Rule{Name: "block-all-" + fam(v6) + "-" + dirName(d), Action: Block, Dir: d, V6: v6, Weight: weightBlock})
		add(Rule{Name: "permit-loopback-" + fam(v6) + "-" + dirName(d), Action: Permit, Dir: d, V6: v6, Iface: IfaceLoopback, Weight: weightPermit})
		add(Rule{Name: "permit-tunnel-" + fam(v6) + "-" + dirName(d), Action: Permit, Dir: d, V6: v6, Iface: IfaceTunnel, Weight: weightPermit})
		for _, port := range dnsPorts {
			add(Rule{Name: fmt.Sprintf("block-dns-%d-%s-%s", port, fam(v6), dirName(d)), Action: Block, Dir: d, V6: v6,
				Iface: IfacePhysical, RemotePort: port, Weight: weightDNS})
		}
	})

	seen := map[Endpoint]bool{}
	for i, e := range p.Endpoints {
		addr := e.Addr.Addr().Unmap()
		switch {
		case !e.Addr.IsValid():
			return nil, fmt.Errorf("%w: endpoint %d is not an address", ErrBadPlan, i)
		case addr.IsUnspecified(), addr.IsLoopback(), addr.IsMulticast(), addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast(),
			addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
			return nil, fmt.Errorf("%w: endpoint %d is a local or special address", ErrBadPlan, i)
		case e.Addr.Port() == 0:
			return nil, fmt.Errorf("%w: endpoint %d has no port", ErrBadPlan, i)
		case e.Proto != ProtoTCP && e.Proto != ProtoUDP:
			return nil, fmt.Errorf("%w: endpoint %d needs TCP or UDP", ErrBadPlan, i)
		}
		for _, dp := range dnsPorts {
			if e.Addr.Port() == dp {
				return nil, fmt.Errorf("%w: endpoint %d uses a DNS port", ErrBadPlan, i)
			}
		}
		for _, pool := range p.TunnelPool {
			if pool.Contains(addr) {
				return nil, fmt.Errorf("%w: endpoint %d is inside the tunnel", ErrBadPlan, i)
			}
		}
		norm := Endpoint{Addr: netip.AddrPortFrom(addr, e.Addr.Port()), Proto: e.Proto}
		if seen[norm] {
			continue
		}
		seen[norm] = true
		for _, d := range []Dir{Out, In} {
			add(Rule{Name: fmt.Sprintf("permit-endpoint-%d-%s", len(seen), dirName(d)), Action: Permit, Dir: d, V6: addr.Is6(),
				Iface: IfacePhysical, Proto: e.Proto, Remote: netip.PrefixFrom(addr, addr.BitLen()),
				RemotePort: e.Addr.Port(), Weight: weightEndpt})
		}
	}

	if p.AllowLAN {
		for _, d := range []Dir{Out, In} {
			for i, pfx := range append(append([]netip.Prefix(nil), lanV4...), lanV6...) {
				add(Rule{Name: fmt.Sprintf("permit-lan-%d-%s", i, dirName(d)), Action: Permit, Dir: d, V6: pfx.Addr().Is6(),
					Iface: IfacePhysical, Remote: pfx, Weight: weightPermit})
			}
		}
	}
	if p.AllowDHCP {
		for _, d := range []Dir{Out, In} {
			for i, pfx := range dhcpV4 {
				add(Rule{Name: fmt.Sprintf("permit-dhcp4-%d-%s", i, dirName(d)), Action: Permit, Dir: d, Iface: IfacePhysical,
					Proto: ProtoUDP, Remote: pfx, RemotePort: 67, LocalPort: 68, Weight: weightPermit})
			}
			for i, pfx := range dhcpV6 {
				add(Rule{Name: fmt.Sprintf("permit-dhcp6-%d-%s", i, dirName(d)), Action: Permit, Dir: d, V6: true, Iface: IfacePhysical,
					Proto: ProtoUDP, Remote: pfx, RemotePort: 547, LocalPort: 546, Weight: weightPermit})
			}
		}
	}
	return rules, nil
}

func (r Rule) matches(p Packet) bool {
	addr := p.Remote.Addr().Unmap()
	switch {
	case r.Dir != p.Dir,
		r.Iface != IfaceAny && r.Iface != p.Iface,
		r.Proto != ProtoAny && r.Proto != p.Proto,
		r.V6 != addr.Is6(),
		r.RemotePort != 0 && r.RemotePort != p.Remote.Port(),
		r.LocalPort != 0 && r.LocalPort != p.Local,
		r.Remote.IsValid() && !r.Remote.Contains(addr):
		return false
	}
	return true
}

// Evaluate returns whether the rules permit a packet: the matching rule of the highest weight,
// Block on a tie, Block when none matches.
func Evaluate(rules []Rule, p Packet) Action {
	best, action := -1, Block
	for _, r := range rules {
		if !r.matches(p) {
			continue
		}
		w := int(r.Weight)
		if w > best || (w == best && r.Action == Block) {
			best, action = w, r.Action
		}
	}
	return action
}
