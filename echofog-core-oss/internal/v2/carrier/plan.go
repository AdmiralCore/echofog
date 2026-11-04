package carrier

import (
	"net/netip"
	"net/url"
	"sort"
	"strings"

	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// AttemptKind says how one attempt reaches the peer.
type AttemptKind uint8

const (
	// AttemptCarrier dials the address with its L0 suite.
	AttemptCarrier AttemptKind = iota
	// AttemptRelay hands the link to a relay (guard mode): the card has no usable direct suite
	// for this side of the border (wire.md §1 D21 rule 4).
	AttemptRelay
)

func (k AttemptKind) String() string {
	if k == AttemptRelay {
		return "relay"
	}
	return "carrier"
}

// Attempt is one step of a carrier plan, in preference order: the first is tried first and the
// second starts after the happy-eyeballs delay (wire.md §1 rule 5).
type Attempt struct {
	Kind  AttemptKind
	Suite uint32
	Addr  *pb.Addr
}

// LocalInfo is what D21 needs to know about the initiator: its public addresses, the built-in
// offline country database, and the network profile flags.
type LocalInfo struct {
	// Addrs are the initiator's public addresses. They are compared with the responder's
	// addresses to establish "same country" (wire.md §1 rule 3); an empty list means every
	// responder is across a border. On mobile they come from the local_addrs callback, never
	// from net.Interfaces in this package.
	Addrs []netip.Addr
	// CountryOf maps an address to an ISO country code using the offline GeoIP database
	// (wire.md §1 rule 3). A nil function or an unknown address counts as a border.
	CountryOf func(netip.Addr) (string, bool)
	// UDPBlocked is the platforms.md §2 udp=blocked flag: only TCP suites are usable.
	UDPBlocked bool
	// Preferred overrides the built-in suite preference order. Suites named here are tried in
	// this order; the rest keep the default order.
	Preferred []uint32
}

// defaultPreference is the order used when LocalInfo.Preferred does not name a suite. Inside one
// country noise3 is the cheapest suite; across a border only the camouflaged suites remain.
var defaultPreference = []uint32{
	objects.SuiteNoise3,
	objects.SuiteReality2QUIC,
	objects.SuiteReality2TCP,
	objects.SuiteWS2,
}

// Plan is the pure implementation of the D21 carrier-selection rules (wire.md §1): it reads only
// the signed card and LocalInfo and spends no network calls. The result is ordered by preference;
// an AttemptRelay is the single element when the card offers no usable direct suite.
func Plan(card *objects.Card, self LocalInfo) []Attempt {
	if card == nil || card.Body == nil {
		return nil
	}
	addrs := card.Addrs()
	domestic := card.Body.GetFlags()&objects.FlagDomesticOnly != 0

	same := make([]bool, len(addrs))
	anySame := false
	for i, a := range addrs {
		same[i] = sameCountry(a, self)
		anySame = anySame || same[i]
	}

	out := make([]Attempt, 0, len(addrs))
	for i, a := range addrs {
		suite := a.GetSuite()
		if !knownSuite(suite) {
			continue
		}
		// Address-level requirements first: an unusable address is skipped regardless of the
		// border rules, so the plan never reports a suite it could not dial.
		switch suite {
		case objects.SuiteNoise3:
			// noise3 is published only in peer and invite cards (objects.md §3.2, wire.md §2.1).
			if card.Scope != objects.ScopePeer && card.Scope != objects.ScopeInvite {
				continue
			}
			if a.GetHost() == "" || a.GetPort() == 0 {
				continue
			}
		case objects.SuiteReality2QUIC, objects.SuiteReality2TCP:
			// No REALITY suite without the cover SNI from the card (wire.md §1 rule 6, V1),
			// and no guessed port: the address must carry both host and port.
			if objects.SuiteParams(a).GetSni() == "" {
				continue
			}
			if a.GetHost() == "" || a.GetPort() == 0 {
				continue
			}
		case objects.SuiteWS2:
			if a.GetUrl() == "" {
				continue
			}
		}

		border := !same[i]
		if self.UDPBlocked && isUDP(suite) {
			continue
		}
		// A card flag domestic-only is never dialed across a border (wire.md §1 rule 1).
		if domestic && border {
			continue
		}
		// Across a border (or an unknown country) only a camouflaged suite is allowed: this
		// covers both the server rule and the ordinary-user rule. noise3 is not camouflaged,
		// so it is confined to one country (wire.md §1 rules 1, 2, 4).
		if border && !isCamouflaged(suite) {
			continue
		}
		out = append(out, Attempt{Kind: AttemptCarrier, Suite: suite, Addr: a})
	}

	if len(out) == 0 {
		// A domestic-only card has no route at all when every address is across the border:
		// even a relay would have to reach it there.
		if domestic && !anySame {
			return nil
		}
		return []Attempt{{Kind: AttemptRelay}}
	}
	return orderAttempts(out, self.Preferred)
}

func knownSuite(s uint32) bool {
	switch s {
	case objects.SuiteNoise3, objects.SuiteReality2QUIC, objects.SuiteReality2TCP, objects.SuiteWS2:
		return true
	}
	return false
}

func isUDP(s uint32) bool {
	return s == objects.SuiteNoise3 || s == objects.SuiteReality2QUIC
}

// IsCamouflaged reports whether the suite carries a cover (wire.md §1: reality2-quic,
// reality2-tcp and ws2).
func IsCamouflaged(s uint32) bool { return isCamouflaged(s) }

func isCamouflaged(s uint32) bool {
	return s == objects.SuiteReality2QUIC || s == objects.SuiteReality2TCP || s == objects.SuiteWS2
}

// orderAttempts sorts attempts by suite preference, keeping the card's address order inside one
// suite.
func orderAttempts(atts []Attempt, preferred []uint32) []Attempt {
	rank := make(map[uint32]int, len(defaultPreference)+len(preferred))
	next := 0
	for _, s := range preferred {
		if _, ok := rank[s]; ok {
			continue
		}
		rank[s] = next
		next++
	}
	for _, s := range defaultPreference {
		if _, ok := rank[s]; ok {
			continue
		}
		rank[s] = next
		next++
	}
	sort.SliceStable(atts, func(i, j int) bool {
		return rank[atts[i].Suite] < rank[atts[j].Suite]
	})
	return atts
}

// sameCountry reports positive knowledge that an address of the card and the initiator are in one
// country. Anything unknown is a border (wire.md §1 rule 3).
func sameCountry(a *pb.Addr, self LocalInfo) bool {
	if self.CountryOf == nil {
		return false
	}
	remote := hostIP(a)
	if !remote.IsValid() {
		return false
	}
	rc, ok := self.CountryOf(remote)
	if !ok {
		return false
	}
	for _, la := range self.Addrs {
		if !la.IsValid() {
			continue
		}
		lc, ok := self.CountryOf(la.Unmap())
		if !ok {
			continue
		}
		if lc == rc {
			return true
		}
	}
	return false
}

// hostIP returns the address of a card address when it is a literal IP. DNS names (including
// ws2 URLs) are unknown to the offline database, so they count as a border.
func hostIP(a *pb.Addr) netip.Addr {
	host := a.GetHost()
	if a.GetSuite() == objects.SuiteWS2 {
		u, err := url.Parse(a.GetUrl())
		if err != nil {
			return netip.Addr{}
		}
		host = u.Hostname()
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}
