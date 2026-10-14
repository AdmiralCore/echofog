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
}

// Mapping says whether the NAT gives us one public address or one per destination.
type Mapping int

const (
	MappingUnknown Mapping = iota // fewer than two independent observations
	MappingStable                 // every observer saw the same address: hole punching can work
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
	// Public is the most commonly observed address (zero when nothing was observed).
	Public netip.AddrPort
	// NoNAT: the observed address belongs to one of our own interfaces, so we are directly
	// reachable and need neither port mapping nor punching.
	NoNAT        bool
	Observations []Observation
}

// Classify turns observations into a Report. local lists our interface addresses.
//
// Only IPv4 observations decide the mapping; IPv6 is normally not translated and a global IPv6
// address on one of our interfaces is handled by NoNAT.
func Classify(obs []Observation, local []netip.Addr) Report {
	r := Report{Observations: obs}

	// An IPv4 and an IPv6 answer are two views of two different networks, not a disagreement.
	// IPv4 decides; IPv6 only when there is no IPv4 observation at all.
	has4 := false
	for _, o := range obs {
		if o.Addr.IsValid() && o.Addr.Port() != 0 && o.Addr.Addr().Unmap().Is4() {
			has4 = true
			break
		}
	}
	counts := map[netip.AddrPort]int{}
	for _, o := range obs {
		if !o.Addr.IsValid() || o.Addr.Port() == 0 {
			continue
		}
		if is4 := o.Addr.Addr().Unmap().Is4(); is4 != has4 {
			continue
		}
		counts[netip.AddrPortFrom(o.Addr.Addr().Unmap(), o.Addr.Port())]++
	}
	if len(counts) == 0 {
		return r
	}

	// the address most observers agree on, ties broken by address so the result is stable
	type kv struct {
		a netip.AddrPort
		n int
	}
	ranked := make([]kv, 0, len(counts))
	total := 0
	for a, n := range counts {
		ranked = append(ranked, kv{a, n})
		total += n
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].n != ranked[j].n {
			return ranked[i].n > ranked[j].n
		}
		return ranked[i].a.String() < ranked[j].a.String()
	})
	r.Public = ranked[0].a

	switch {
	case total < 2:
		r.Mapping = MappingUnknown
	case len(counts) == 1:
		r.Mapping = MappingStable
	default:
		r.Mapping = MappingVaries
	}

	for _, ip := range local {
		if ip.Unmap() == r.Public.Addr().Unmap() {
			r.NoNAT = true
			break
		}
	}
	return r
}
