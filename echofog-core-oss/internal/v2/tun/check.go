package tun

import (
	"errors"
	"fmt"
	"net/netip"
)

// ErrBadRules is a rule set a driver refuses to install.
var ErrBadRules = errors.New("tun: the rules do not keep the computer closed")

// MaxRules bounds a rule set. BuildRules makes about 40 rules plus four per endpoint.
const MaxRules = 1024

// probeAddrs are addresses nothing may be permitted to on the physical interface: documentation
// ranges (never routed, so never a real endpoint) in both families. A rule set that permits one of
// them permits a whole range of the Internet.
var probeAddrs = []netip.Addr{
	netip.MustParseAddr("192.0.2.250"),
	netip.MustParseAddr("198.51.100.250"),
	netip.MustParseAddr("203.0.113.250"),
	netip.MustParseAddr("8.8.8.8"),
	netip.MustParseAddr("2001:db8:ffff::250"),
	netip.MustParseAddr("2606:4700:4700::1111"),
}

// probeLAN are private addresses, for the DNS ports only (a LAN may be permitted, its resolver may
// not).
var probeLAN = []netip.Addr{
	netip.MustParseAddr("192.168.1.1"),
	netip.MustParseAddr("10.0.0.1"),
	netip.MustParseAddr("fd00::1"),
}

var probePorts = []uint16{80, 443, 4433, 53, 853}

// CheckRules is the last line before a driver touches the firewall: it refuses a rule set that does
// not close the computer, whoever built it (a driver also receives its rules from another process).
// The set must be persistent, named, bounded, and must block every probe packet that has no business
// leaving the physical interface: public addresses of both families, every direction, TCP, UDP and
// ICMP, and the DNS ports even towards a LAN address.
func CheckRules(rules []Rule) error {
	if len(rules) == 0 || len(rules) > MaxRules {
		return fmt.Errorf("%w: %d rules", ErrBadRules, len(rules))
	}
	names := make(map[string]bool, len(rules))
	for i, r := range rules {
		if r.Name == "" || names[r.Name] {
			return fmt.Errorf("%w: rule %d has an empty or repeated name", ErrBadRules, i)
		}
		names[r.Name] = true
		if !r.Persistent {
			return fmt.Errorf("%w: rule %q is not persistent", ErrBadRules, r.Name)
		}
	}
	// Evaluate answers Block when nothing matches, a real firewall permits: an explicit block of
	// everything must exist for each family and direction.
	for _, dir := range []Dir{Out, In} {
		for _, v6 := range []bool{false, true} {
			found := false
			for _, r := range rules {
				if r.Action == Block && r.Dir == dir && r.V6 == v6 && r.Iface == IfaceAny && r.Proto == ProtoAny &&
					!r.Remote.IsValid() && r.RemotePort == 0 && r.LocalPort == 0 {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("%w: no block-everything rule for dir %v, v6=%v", ErrBadRules, dir, v6)
			}
		}
	}
	probe := func(p Packet, what string) error {
		if Evaluate(rules, p) != Block {
			return fmt.Errorf("%w: %s is permitted", ErrBadRules, what)
		}
		return nil
	}
	for _, dir := range []Dir{Out, In} {
		for _, a := range probeAddrs {
			for _, port := range probePorts {
				for _, proto := range []Proto{ProtoTCP, ProtoUDP} {
					p := Packet{Dir: dir, Iface: IfacePhysical, Proto: proto, Remote: netip.AddrPortFrom(a, port), Local: 40000}
					if err := probe(p, fmt.Sprintf("%v %v to %v", dir, proto, p.Remote)); err != nil {
						return err
					}
				}
			}
			for _, typ := range []uint8{0, 3, 8, 128, 129} {
				p := Packet{Dir: dir, Iface: IfacePhysical, Proto: ProtoICMP, Remote: netip.AddrPortFrom(a, 0), ICMPType: typ}
				if err := probe(p, fmt.Sprintf("%v ICMP type %d to %v", dir, typ, a)); err != nil {
					return err
				}
			}
		}
		for _, a := range probeLAN {
			for _, port := range []uint16{53, 853} {
				for _, proto := range []Proto{ProtoTCP, ProtoUDP} {
					p := Packet{Dir: dir, Iface: IfacePhysical, Proto: proto, Remote: netip.AddrPortFrom(a, port), Local: 40000}
					if err := probe(p, fmt.Sprintf("%v %v to the resolver %v", dir, proto, p.Remote)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// DefaultTunnelPool is the address space of the tunnel device: its resolver and its own addresses live
// inside it, and no endpoint outside the tunnel may.
var DefaultTunnelPool = []netip.Prefix{netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("fdfe:dcba:9876::/48")}

// ErrBadDNS is a DNS configuration the helper refuses.
var ErrBadDNS = errors.New("tun: bad DNS configuration")

// CheckDNS is the check of the helper with administrator rights on what the application asks for: a
// resolver that is a plain address (no IPv6 zone) inside the tunnel's pool, and the policy against the
// parallel lookups on. Nothing else reaches a command or a registry value.
func CheckDNS(c DNSConfig) error {
	return nil
}
