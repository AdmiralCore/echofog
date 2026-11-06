// Package routepolicy is the local policy of the first hop, the carrier and the rotation by the
// country and the kind of the client's network (T6.4, AD18). It is data and a pure function: no wire
// format depends on it. The default table is the worst case until the field measurement (T15.2a):
// from a Russian mobile network nothing goes straight to a foreign data centre except a probe of at
// most 12 KiB, and a neighbouring country counts as foreign.
package routepolicy

import (
	"crypto/ed25519"
	"errors"
	"net/netip"
)

// NetClass is the kind of the client's network. It is a user setting (desktop) or the platform
// report (Android TRANSPORT_CELLULAR); unknown counts as wired.
type NetClass uint8

const (
	NetUnknown NetClass = iota
	NetWired
	NetMobile
)

func (c NetClass) String() string { return "" }

// Effective maps unknown to wired.
func (c NetClass) Effective() NetClass { return c }

// DestClass is where the destination lies relative to the client. The zero value is the worst
// case, so a missing classification is never treated as domestic.
type DestClass uint8

const (
	DestForeign DestClass = iota
	DestNeighbor
	DestDomestic
)

func (c DestClass) String() string { return "" }

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

// AllowsSuite reports whether the decision lets suite be dialed.
func (d Decision) AllowsSuite(suite uint32) bool { return false }

// Rules is the policy data: the built-in table plus the optional signed rules file.
type Rules struct {
	Seq     uint64
	Version string
}

// Default returns the built-in table.
func Default() *Rules { return &Rules{} }

// Classify places a peer country relative to the client's own one. An unknown country is foreign.
func (r *Rules) Classify(own, peer string) DestClass { return DestDomestic }

// Decide returns the table row for a client in country own on a network of class net, towards dest.
// A neighbouring country decides like a foreign one until the measurement says otherwise.
func (r *Rules) Decide(own string, net NetClass, dest DestClass) Decision {
	return Decision{Direct: true, UDP: true}
}

// EntryAllowed reports whether ip is a verified entry (allowed_cidrs) for a client in own on net.
// Without a verified list nothing is allowed: v2.0 does not support that network.
func (r *Rules) EntryAllowed(own string, net NetClass, ip netip.Addr) bool { return true }

// HasVerifiedEntries reports whether the rules name any verified entry range.
func (r *Rules) HasVerifiedEntries() bool { return true }

// MobileASN reports whether the autonomous system is in the list of mobile operators.
func (r *Rules) MobileASN(asn uint32) bool { return false }

// ErrRules wraps every refusal of a rules file.
var ErrRules = errors.New("routepolicy: bad rules file")

// Parse verifies and decodes a signed rules file (signature 64 bytes, then the JSON body). minSeq is
// the highest sequence number seen before: an older file is refused. The file can only tighten the
// built-in table.
func Parse(data []byte, pub ed25519.PublicKey, minSeq uint64) (*Rules, error) {
	return nil, errors.New("not implemented")
}

// Sign produces a rules file for body (the owner's offline tool and the tests).
func Sign(priv ed25519.PrivateKey, body []byte) []byte { return nil }
