// Package routepolicy is the local policy of the first hop, the carrier and the rotation by the
// country and the kind of the client's network (T6.4, AD18). It is data and a pure function: no wire
// format depends on it. The default table is the worst case until the field measurement (T15.2a):
// from a Russian mobile network nothing goes straight to a foreign data centre except a probe of at
// most 12 KiB, and a neighbouring country counts as foreign.
package routepolicy

import (
	"net/netip"
	"slices"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// NetClass is the kind of the client's network. It is a user setting (desktop) or the platform
// report (Android TRANSPORT_CELLULAR); unknown counts as wired.
type NetClass uint8

const (
	NetUnknown NetClass = iota
	NetWired
	NetMobile
)

func (c NetClass) String() string {
	switch c {
	case NetWired:
		return "wired"
	case NetMobile:
		return "mobile"
	}
	return "unknown"
}

// Effective maps unknown to wired.
func (c NetClass) Effective() NetClass {
	if c == NetUnknown {
		return NetWired
	}
	return c
}

// DestClass is where the destination lies relative to the client. The zero value is the worst
// case, so a missing classification is never treated as domestic.
type DestClass uint8

const (
	DestForeign DestClass = iota
	DestNeighbor
	DestDomestic
)

func (c DestClass) String() string {
	switch c {
	case DestNeighbor:
		return "neighbor"
	case DestDomestic:
		return "domestic"
	}
	return "foreign"
}

// ProbeBytes is the most a probe connection to a foreign data centre may carry from a network where
// TCP freezes after 16–20 KiB (decisions.md, freeze_safe_bytes).
const ProbeBytes = 12288

// Decision is the outcome of one table row.
type Decision struct {
	// RuleID names the row, for the connection report (T11.2).
	RuleID string
	// Direct: normal sessions may use a direct carrier to the destination.
	Direct bool
	// Suites are the allowed L0 suites in order; nil means no restriction beyond D21.
	Suites []uint32
	// TCPLimit is the most bytes one TCP carrier to the destination may move; 0 is unlimited.
	TCPLimit int
	// UDP: a UDP suite may be tried towards the destination.
	UDP bool
	// Rotate: exits and large transfers rotate the path (H7).
	Rotate bool
	// EntryInCountry: the first hop must be an entry node in the client's own country.
	EntryInCountry bool
	// Probe is the budget of a probe connection when Direct is false; 0 means no probe.
	Probe int
}

// AllowsSuite reports whether the decision lets suite be dialed: it is in the list (when there is
// one) and, for the UDP suites, UDP is allowed.
func (d Decision) AllowsSuite(suite uint32) bool {
	if d.Suites != nil && !slices.Contains(d.Suites, suite) {
		return false
	}
	if !d.UDP && IsUDP(suite) {
		return false
	}
	return true
}

// IsUDP reports whether the suite runs over UDP.
func IsUDP(suite uint32) bool {
	return suite == objects.SuiteNoise3 || suite == objects.SuiteReality2QUIC
}

// Rules is the policy data: the built-in table plus the optional signed rules file.
type Rules struct {
	Seq     uint64
	Version string

	allowed   []netip.Prefix      // verified entry ranges (allowed_cidrs, H14)
	mobileASN []uint32            // mobile operators (a desktop without TRANSPORT_CELLULAR)
	neighbors map[string][]string // country → neighbouring countries (data for the report only)
	tcpLimit  int                 // file: extra cap on foreign TCP, 0 = none
	udpDenied []string            // file: countries where UDP towards abroad is off
}

// Default returns the built-in table.
func Default() *Rules { return &Rules{Version: "builtin-1"} }

// Classify places a peer country relative to the client's own one. An unknown country is foreign.
func (r *Rules) Classify(own, peer string) DestClass {
	if own == "" || peer == "" {
		return DestForeign
	}
	if own == peer {
		return DestDomestic
	}
	if r != nil && slices.Contains(r.neighbors[own], peer) {
		return DestNeighbor
	}
	return DestForeign
}

var (
	allSuites = []uint32{objects.SuiteNoise3, objects.SuiteReality2QUIC, objects.SuiteReality2TCP, objects.SuiteWS2}
	// Towards abroad only the camouflaged suites exist anyway (wire.md §1); the CN row writes that out.
	cnForeign = []uint32{objects.SuiteReality2QUIC, objects.SuiteReality2TCP, objects.SuiteWS2}
	// IR: 53/80/443 and an allow-list of services: TCP suites at 443; QUIC only when udp is allowed.
	irForeign = []uint32{objects.SuiteReality2TCP, objects.SuiteWS2}
	// Unknown own country: what a censor cannot tell from a browser, over TCP.
	unknownRow = []uint32{objects.SuiteReality2TCP, objects.SuiteWS2}
)

// Decide returns the table row for a client in country own on a network of class net, towards dest.
// A neighbouring country decides like a foreign one until the measurement says otherwise.
func (r *Rules) Decide(own string, net NetClass, dest DestClass) Decision {
	net = net.Effective()
	if dest == DestNeighbor {
		dest = DestForeign
	}
	var d Decision
	switch {
	case own == "":
		// The country is not known (T6.4c): TLS-shaped carriers over TCP only, whatever the network;
		// no noise3, no QUIC, no UDP, no hole punching. Not stricter for a mobile network by itself.
		d = Decision{RuleID: "unknown", Direct: true, Suites: slices.Clone(unknownRow)}
	case dest == DestDomestic && own == "RU" && net == NetMobile:
		d = Decision{RuleID: "ru-mobile-domestic", Direct: true, Suites: slices.Clone(allSuites), UDP: true}
	case dest == DestDomestic:
		d = Decision{RuleID: "domestic", Direct: true, UDP: true}
	case own == "RU" && net == NetMobile:
		d = Decision{RuleID: "ru-mobile-foreign", Direct: false, TCPLimit: ProbeBytes, Probe: ProbeBytes,
			EntryInCountry: true}
	case own == "RU":
		d = Decision{RuleID: "ru-wired-foreign", Direct: true, UDP: true, Rotate: true}
	case own == "CN":
		d = Decision{RuleID: "cn-foreign", Direct: true, Suites: slices.Clone(cnForeign), UDP: true, Rotate: true}
	case own == "IR":
		d = Decision{RuleID: "ir-foreign", Direct: true, Suites: slices.Clone(irForeign), Rotate: true}
	default:
		d = Decision{RuleID: "foreign", Direct: true, UDP: true, Rotate: true}
	}
	if dest != DestDomestic && r != nil {
		if r.tcpLimit > 0 && (d.TCPLimit == 0 || r.tcpLimit < d.TCPLimit) {
			d.TCPLimit = r.tcpLimit
			if d.Probe > 0 {
				d.Probe = r.tcpLimit
			}
		}
		if slices.Contains(r.udpDenied, own) {
			d.UDP = false
		}
	}
	return d
}

// EntryAllowed reports whether ip is a verified entry (allowed_cidrs) for a client in own on net.
// Without a verified list nothing is allowed: v2.0 does not support that network (H14).
func (r *Rules) EntryAllowed(own string, net NetClass, ip netip.Addr) bool {
	if r == nil || !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	for _, p := range r.allowed {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// HasVerifiedEntries reports whether the rules name any verified entry range.
func (r *Rules) HasVerifiedEntries() bool { return r != nil && len(r.allowed) > 0 }

// MobileASN reports whether the autonomous system is in the list of mobile operators.
func (r *Rules) MobileASN(asn uint32) bool { return r != nil && slices.Contains(r.mobileASN, asn) }
