// Package natcheck finds out what our NAT does: which public address the world sees for our UDP
// socket and whether that address stays the same for every destination.
//
// The rule that tells a hole-punchable NAT from an unpunchable one is taken from Tailscale's
// netcheck (github.com/tailscale/tailscale, net/netcheck, BSD-3-Clause, Copyright (c) 2020
// Tailscale Inc & contributors): ask several independent parties which address they see; if the
// answers differ, the mapping depends on the destination ("hard" or symmetric NAT) and a direct
// connection cannot be punched through it. See THIRD_PARTY_NOTICES.md.
package natcheck

import (
	"net/netip"
	"sort"
)

// Observation is the address one remote party (a STUN server or a connected peer) saw our
// socket at.
type Observation struct {
	From string         // who observed: STUN server host:port or peer ID
	Addr netip.AddrPort // what they saw
	// IPOnly: the observer is connected through another socket of ours (a REALITY dial opens its
	// own), which the NAT gave a mapping of its own. Its IP is ours all the same, its port says
	// nothing about the socket peers reach us at: it counts toward the public IP (and the country
	// placed from it) but not toward the port, the mapping or the port range.
	IPOnly bool
}

// Mapping says whether the NAT gives us one public address or one per destination.
type Mapping int

const (
	MappingUnknown Mapping = iota // fewer than two independent observations, or no telling
	MappingStable                 // the observers saw the same address: hole punching can work
	MappingVaries                 // observers saw different addresses: symmetric NAT, relay only
)

func (m Mapping) String() string {
	switch m {
	case MappingStable:
		return "stable"
	case MappingVaries:
		return "varies by destination"
	default:
		return "unknown"
	}
}

// Report is the result of classifying a set of observations.
type Report struct {
	Mapping Mapping
	// Public is the address the observers agree on: its IP is the one more independent observers
	// saw than any other, its port the one most of those saw. It is zero when nothing was observed
	// or when two IPs are tied: a tie is not decided at all (it used to go to the lower address as
	// a string, so one lying peer out of two chose our address).
	Public netip.AddrPort
	// Agree is how many independent observers saw Public's IP, out of Observers that answered in
	// its address family (IPOnly ones included). Public's port is zero when only IPOnly observers
	// saw that IP.
	Agree, Observers int
	// NoNAT: the observed address belongs to one of our own interfaces, so we are directly
	// reachable and need neither port mapping nor punching.
	NoNAT        bool
	Observations []Observation
	// PortLo and PortHi bound the ports observed on Public's IP (IPv4 only), and Ports counts the
	// different ones. Behind a symmetric NAT they show where the NAT takes its ports from: a carrier
	// NAT that hands each subscriber a block of ports keeps them close together, which tells a
	// peer aiming a birthday punch at us where to aim. A port far from the others is left out (see
	// portWindow): one lying observer must not stretch the range over every port.
	PortLo, PortHi uint16
	Ports          int
}

// Agreed reports whether at least n independent observers saw Public's IP and they are more
// than half of those that answered.
func (r Report) Agreed(n int) bool {
	return r.Public.IsValid() && r.Agree >= n && 2*r.Agree > r.Observers
}

// reserved are the ranges PublicAddr refuses besides the private, loopback, link-local,
// unspecified and multicast ones netip knows: they are never routed on the Internet, so an
// observation of one says nothing about how the Internet sees us (and two of them next to the
// public answers would make a cone NAT look symmetric).
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments (DS-Lite B4: 192.0.0.2)
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking, used inside some carriers
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and 255.255.255.255
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("3fff::/20"),       // documentation (RFC 9637)
}

// PublicAddr reports whether a is an address the Internet routes to: not private, loopback,
// link-local, unspecified, multicast, or one of the reserved ranges (carrier-grade NAT, test and
// documentation networks, benchmarking, 240/4, ...). An observation of such an address comes from
// a peer on the same network, or inside the same carrier's NAT, and says nothing about how the
// Internet sees us.
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// portWindow is how far from the middle of the observed ports a port may lie and still count
// toward PortLo/PortHi: a carrier NAT's port block is a few thousand ports at most, and a NAT that
// picks ports anywhere spreads them so widely that the window grows with them.
const portWindow = 2048

// Classify turns observations into a Report. local lists our interface addresses.
//
// Every observer has one vote (the first answer of each counts). Only IPv4 observations decide
// the mapping; IPv6 is normally not translated and a global IPv6 address on one of our interfaces
// is handled by NoNAT. When some observers saw a public address, the ones that saw a private one (a
// peer on our LAN, or inside the same carrier NAT) are left out: they look at another side of the
// NAT, and counting them would make a cone NAT look symmetric. With no public observation at all (a
// closed network) every one counts.
//
// Lying observers are expected. The public IP is the one most observers saw, and a tie decides
// nothing. The mapping "varies" only when the answers are spread over three addresses or more, so
// at least two observers dispute any one of them: an observer that disagrees with the rest is
// outvoted, and one against one is a doubt (MappingUnknown), not a symmetric NAT.
func Classify(obs []Observation, local []netip.Addr) Report {
	r := Report{Observations: obs}

	voted := map[string]bool{}
	valid := make([]Observation, 0, len(obs))
	for _, o := range obs {
		if !o.Addr.IsValid() || (o.Addr.Port() == 0 && !o.IPOnly) || voted[o.From] {
			continue
		}
		voted[o.From] = true
		valid = append(valid, Observation{From: o.From, Addr: netip.AddrPortFrom(o.Addr.Addr().Unmap(), o.Addr.Port()), IPOnly: o.IPOnly})
	}

	anyPublic := false
	for _, o := range valid {
		if PublicAddr(o.Addr.Addr()) {
			anyPublic = true
			break
		}
	}
	if anyPublic {
		kept := make([]Observation, 0, len(valid))
		for _, o := range valid {
			if PublicAddr(o.Addr.Addr()) {
				kept = append(kept, o)
			}
		}
		valid = kept
	}

	// An IPv4 and an IPv6 answer are two views of two different networks, not a disagreement.
	// IPv4 decides; IPv6 only when there is no IPv4 observation at all.
	has4 := false
	for _, o := range valid {
		if o.Addr.Addr().Is4() {
			has4 = true
			break
		}
	}
	fam := make([]Observation, 0, len(valid))
	for _, o := range valid {
		if o.Addr.Addr().Is4() == has4 {
			fam = append(fam, o)
		}
	}
	if len(fam) == 0 {
		return r
	}
	ported := make([]Observation, 0, len(fam)) // the ones whose port counts as well
	for _, o := range fam {
		if !o.IPOnly {
			ported = append(ported, o)
		}
	}
	r.Observers = len(fam)
	r.Mapping = mappingOf(ported)

	ip, agree, tied := majorityIP(fam)
	if tied {
		return r // no address wins: the caller asks more observers or keeps what it had
	}
	r.Agree = agree
	r.Public = netip.AddrPortFrom(ip, commonPort(ported, ip))
	if ip.Is4() && r.Public.Port() != 0 {
		r.PortLo, r.PortHi, r.Ports = portRange(ported, ip, r.Public.Port())
	}

	for _, l := range local {
		if l.Unmap() == ip {
			r.NoNAT = true
			break
		}
	}
	return r
}

// mappingOf judges the mapping from one family's observations, one per observer.
func mappingOf(fam []Observation) Mapping {
	n := len(fam)
	if n < 2 {
		return MappingUnknown
	}
	votes := map[netip.AddrPort]int{}
	top := 0
	for _, o := range fam {
		votes[o.Addr]++
		top = max(top, votes[o.Addr])
	}
	switch {
	case 2*top > n:
		return MappingStable // everyone agrees, or the few who do not are outvoted
	case len(votes) >= 3:
		return MappingVaries // every address is disputed by two observers or more
	default:
		return MappingUnknown // one against one, or two against two: a lie looks just like a NAT
	}
}

// majorityIP is the IP more observers saw than any other, with their number; tied when two IPs
// share the top.
func majorityIP(fam []Observation) (ip netip.Addr, n int, tied bool) {
	votes := map[netip.Addr]int{}
	for _, o := range fam {
		votes[o.Addr.Addr()]++
	}
	for a, v := range votes {
		switch {
		case v > n:
			ip, n, tied = a, v, false
		case v == n:
			tied = true
		}
	}
	return ip, n, tied
}

// commonPort is the port most observers of ip saw; between equals, the one of the earliest
// observation (callers list their most trusted observers first).
func commonPort(fam []Observation, ip netip.Addr) uint16 {
	votes := map[uint16]int{}
	best, bestN := uint16(0), 0
	for _, o := range fam {
		if o.Addr.Addr() != ip {
			continue
		}
		p := o.Addr.Port()
		votes[p]++
		if votes[p] > bestN {
			best, bestN = p, votes[p]
		}
	}
	return best
}

// portRange bounds the ports observed on ip, leaving out the ones far from the middle: the median
// of three answers or more, else the agreed port center.
func portRange(fam []Observation, ip netip.Addr, center uint16) (lo, hi uint16, n int) {
	var ports []int
	for _, o := range fam {
		if o.Addr.Addr() == ip {
			ports = append(ports, int(o.Addr.Port()))
		}
	}
	mid, window := int(center), portWindow
	if len(ports) >= 3 {
		sort.Ints(ports)
		mid = ports[len(ports)/2]
		dev := make([]int, len(ports))
		for i, p := range ports {
			dev[i] = abs(p - mid)
		}
		sort.Ints(dev)
		window = max(window, 4*dev[len(dev)/2])
	}
	seen := map[int]bool{}
	for _, p := range ports {
		if abs(p-mid) > window || seen[p] {
			continue
		}
		seen[p] = true
		if n == 0 || uint16(p) < lo {
			lo = uint16(p)
		}
		hi = max(hi, uint16(p))
		n++
	}
	return lo, hi, n
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
