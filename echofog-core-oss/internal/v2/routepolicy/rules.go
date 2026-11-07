package routepolicy

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"time"
)

// ErrRules wraps every refusal of a rules file.
var ErrRules = errors.New("routepolicy: bad rules file")

const (
	// rulesLabel separates the rules key's signatures from any other use of the key (AD18).
	rulesLabel = "echofog/v2/country-rules/1\x00"
	// MaxRulesFile bounds the file before anything is decoded.
	MaxRulesFile = 64 << 10
	maxCIDRs     = 256
	// minBits4 and minBits6 are the widest verified range a file may name: "the whole internet" is
	// not an entry list.
	minBits4     = 8
	minBits6     = 16
	maxASN       = 1024
	maxNeighbors = 64
)

var countryRE = regexp.MustCompile(`^[A-Z]{2}$`)

// rulesBody is the JSON body of the file. Unknown fields are ignored (a newer file may carry more).
type rulesBody struct {
	Seq          uint64              `json:"seq"`
	Version      string              `json:"version"`
	AllowedCIDRs []string            `json:"allowed_cidrs"`
	MobileASN    []uint32            `json:"mobile_asn"`
	Neighbors    map[string][]string `json:"neighbors"`
	TCPLimit     int                 `json:"tcp_limit"`
	UDPDenied    []string            `json:"udp_denied"`
	ValidUntil   int64               `json:"valid_until"`
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRules, fmt.Sprintf(format, a...))
}

// parse verifies and decodes a signed rules file (signature 64 bytes, then the JSON body). minSeq is
// the highest sequence number seen before: an older file is refused. The file can only tighten the
// built-in table: a neighbour listed in it still decides like a foreign country, a limit above the
// table's own does nothing and nothing in it enables a carrier.
func parse(data []byte, pub ed25519.PublicKey, minSeq uint64) (*Rules, *rulesBody, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, nil, bad("no rules key")
	}
	if len(data) <= ed25519.SignatureSize || len(data) > MaxRulesFile {
		return nil, nil, bad("size %d", len(data))
	}
	sig, body := data[:ed25519.SignatureSize], data[ed25519.SignatureSize:]
	if !ed25519.Verify(pub, append([]byte(rulesLabel), body...), sig) {
		return nil, nil, bad("signature")
	}
	var b rulesBody
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&b); err != nil {
		return nil, nil, bad("json: %v", err)
	}
	if b.Seq < minSeq {
		return nil, nil, bad("seq %d is older than %d", b.Seq, minSeq)
	}
	r := &Rules{Seq: b.Seq, Version: b.Version, mobileASN: b.MobileASN}
	if len(b.Version) > 64 {
		return nil, nil, bad("version too long")
	}
	if len(b.AllowedCIDRs) > maxCIDRs || len(b.MobileASN) > maxASN || len(b.Neighbors) > maxNeighbors {
		return nil, nil, bad("too many entries")
	}
	for _, s := range b.AllowedCIDRs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, nil, bad("allowed_cidrs %q", s)
		}
		if (p.Addr().Is4() && p.Bits() < minBits4) || (!p.Addr().Is4() && p.Bits() < minBits6) {
			return nil, nil, bad("allowed_cidrs %q is too wide (at least /%d for IPv4, /%d for IPv6)", s, minBits4, minBits6)
		}
		r.allowed = append(r.allowed, p.Masked())
	}
	if len(b.Neighbors) > 0 {
		r.neighbors = map[string][]string{}
	}
	for c, list := range b.Neighbors {
		if !countryRE.MatchString(c) || len(list) > maxNeighbors {
			return nil, nil, bad("neighbors of %q", c)
		}
		for _, n := range list {
			if !countryRE.MatchString(n) {
				return nil, nil, bad("neighbor %q", n)
			}
		}
		r.neighbors[c] = append([]string(nil), list...)
	}
	for _, c := range b.UDPDenied {
		if !countryRE.MatchString(c) {
			return nil, nil, bad("udp_denied %q", c)
		}
	}
	if len(b.UDPDenied) > maxNeighbors {
		return nil, nil, bad("too many entries")
	}
	if b.TCPLimit < 0 {
		return nil, nil, bad("tcp_limit %d", b.TCPLimit)
	}
	r.tcpLimit, r.udpDenied = b.TCPLimit, append([]string(nil), b.UDPDenied...)
	return r, &b, nil
}

// Sign produces a rules file for body (the owner's offline tool and the tests).
func Sign(priv ed25519.PrivateKey, body []byte) []byte {
	sig := ed25519.Sign(priv, append([]byte(rulesLabel), body...))
	return append(sig, body...)
}

// MaxValidity is the longest valid_until a deployed rules file may carry, counted from the moment it
// is read. The curator signs 30 days; the slack covers a late update, a longer term is a mistake.
const MaxValidity = 45 * 24 * time.Hour

// ParseAt is Parse for a deployed file. valid_until (unix seconds) is required and at most
// MaxValidity ahead of now(); the file remembers it and now, and a verified entry (allowed_cidrs)
// stops counting when it passes. An expired file never loosens anything: its tcp_limit and
// udp_denied stay in force.
func ParseAt(data []byte, pub ed25519.PublicKey, minSeq uint64, now func() time.Time) (*Rules, error) {
	if now == nil {
		now = time.Now
	}
	r, b, err := parse(data, pub, minSeq)
	if err != nil {
		return nil, err
	}
	if b.ValidUntil <= 0 {
		return nil, bad("valid_until is required")
	}
	// Integers, not times: time.Unix wraps near MaxInt64 and would slip past a time comparison.
	if b.ValidUntil > now().Unix()+int64(MaxValidity/time.Second) {
		return nil, bad("valid_until is more than %s ahead", MaxValidity)
	}
	until := time.Unix(b.ValidUntil, 0)
	r.validUntil, r.now = until, now
	return r, nil
}

// ValidUntil is the end of the file's validity; zero for a file read with Parse.
func (r *Rules) ValidUntil() time.Time {
	if r == nil {
		return time.Time{}
	}
	return r.validUntil
}

// Expired reports whether the file is past valid_until (at that moment or later).
func (r *Rules) Expired() bool {
	return r != nil && !r.validUntil.IsZero() && r.now != nil && !r.now().Before(r.validUntil)
}
