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
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRules, fmt.Sprintf(format, a...))
}

// Parse verifies and decodes a signed rules file (signature 64 bytes, then the JSON body). minSeq is
// the highest sequence number seen before: an older file is refused. The file can only tighten the
// built-in table: a neighbour listed in it still decides like a foreign country, a limit above the
// table's own does nothing and nothing in it enables a carrier.
func Parse(data []byte, pub ed25519.PublicKey, minSeq uint64) (*Rules, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, bad("no rules key")
	}
	if len(data) <= ed25519.SignatureSize || len(data) > MaxRulesFile {
		return nil, bad("size %d", len(data))
	}
	sig, body := data[:ed25519.SignatureSize], data[ed25519.SignatureSize:]
	if !ed25519.Verify(pub, append([]byte(rulesLabel), body...), sig) {
		return nil, bad("signature")
	}
	var b rulesBody
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&b); err != nil {
		return nil, bad("json: %v", err)
	}
	if b.Seq < minSeq {
		return nil, bad("seq %d is older than %d", b.Seq, minSeq)
	}
	r := &Rules{Seq: b.Seq, Version: b.Version, mobileASN: b.MobileASN}
	if len(b.Version) > 64 {
		return nil, bad("version too long")
	}
	if len(b.AllowedCIDRs) > maxCIDRs || len(b.MobileASN) > maxASN || len(b.Neighbors) > maxNeighbors {
		return nil, bad("too many entries")
	}
	for _, s := range b.AllowedCIDRs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, bad("allowed_cidrs %q", s)
		}
		r.allowed = append(r.allowed, p.Masked())
	}
	if len(b.Neighbors) > 0 {
		r.neighbors = map[string][]string{}
	}
	for c, list := range b.Neighbors {
		if !countryRE.MatchString(c) || len(list) > maxNeighbors {
			return nil, bad("neighbors of %q", c)
		}
		for _, n := range list {
			if !countryRE.MatchString(n) {
				return nil, bad("neighbor %q", n)
			}
		}
		r.neighbors[c] = append([]string(nil), list...)
	}
	for _, c := range b.UDPDenied {
		if !countryRE.MatchString(c) {
			return nil, bad("udp_denied %q", c)
		}
	}
	if len(b.UDPDenied) > maxNeighbors {
		return nil, bad("too many entries")
	}
	if b.TCPLimit < 0 {
		return nil, bad("tcp_limit %d", b.TCPLimit)
	}
	r.tcpLimit, r.udpDenied = b.TCPLimit, append([]string(nil), b.UDPDenied...)
	return r, nil
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
	return nil, bad("not implemented")
}

// ValidUntil is the end of the file's validity; zero for a file read with Parse.
func (r *Rules) ValidUntil() time.Time { return time.Time{} }

// Expired reports whether the file is past valid_until.
func (r *Rules) Expired() bool { return false }
