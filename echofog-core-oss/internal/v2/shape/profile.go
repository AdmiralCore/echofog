package shape

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/dns/dnsmessage"

	objectspb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Shape profile IDs (registries.md §6.3).
const (
	IDNone             uint32 = 0
	IDDefaultUDP       uint32 = 1
	IDDefaultTLS       uint32 = 2
	IDConstantRateCall uint32 = 3
)

// Decoy kinds (wire.md §9).
const (
	DecoyRaw         uint32 = 1
	DecoyDNS         uint32 = 2
	DecoyQUICInitial uint32 = 3
)

// Errors returned when a profile is read. A card with such a profile is usable only through a
// different address, because nothing here may guess what the sender meant.
var (
	ErrUnknownProfile  = errors.New("shape: unknown built-in profile")
	ErrReservedProfile = errors.New("shape: reserved built-in profile")
	ErrInvalidProfile  = errors.New("shape: invalid profile")
)

// Limits from wire.md §9, §10 and the datagram layout of §2.4 (see doc.go).
const (
	minInitLen = 1271
	maxInitLen = 1452

	maxPad     = 16384
	maxBuckets = 32

	maxJunkCount = 16
	maxJunkLen   = 1452
	maxJunkGapMs = 1000

	maxDecoys     = 16
	maxDecoyBytes = 1452

	minKeepaliveS = 1
	maxKeepaliveS = 59

	defaultKeepaliveMin = 12
	defaultKeepaliveMax = 24
)

// Range is an inclusive range of a shape profile field (wire.md §9).
type Range struct {
	Min, Max uint32
}

// Junk describes the junk datagrams sent before the first noise3 Init (wire.md §2.7, §9).
type Junk struct {
	Count Range
	Len   Range
	Gap   Range // milliseconds
}

// Decoy is one datagram shaped like something else, sent before the first Init (wire.md §2.7, §9).
type Decoy struct {
	Kind     uint32
	Template []byte // kind 1: bytes with 0xff…ff runs replaced
	QName    string // kind 2: the query name
}

// Profile is a validated shape profile. Pointer fields are absent when the profile does not set
// them; Keepalive always has the §7.1 default when the profile is built by Builtin or Parse.
type Profile struct {
	ID        uint32
	InitLen   *Range
	Pad       *Range
	Buckets   []uint32
	PadBudget *Range
	Junk      *Junk
	Decoys    []Decoy
	Keepalive Range
	IdlePad   *Range
}

var (
	noneProfile = Profile{
		ID:        IDNone,
		Keepalive: Range{defaultKeepaliveMin, defaultKeepaliveMax},
	}
	defaultUDPProfile = Profile{
		ID:        IDDefaultUDP,
		InitLen:   &Range{1280, 1420},
		Pad:       &Range{0, 48},
		Junk:      &Junk{Count: Range{0, 2}, Len: Range{32, 256}, Gap: Range{5, 30}},
		Keepalive: Range{defaultKeepaliveMin, defaultKeepaliveMax},
		IdlePad:   &Range{500, 4000},
	}
	defaultTLSProfile = Profile{
		ID:        IDDefaultTLS,
		Pad:       &Range{0, 256},
		Buckets:   []uint32{512, 1024, 1460, 2920, 4096},
		PadBudget: &Range{4096, 16384},
		Keepalive: Range{defaultKeepaliveMin, defaultKeepaliveMax},
		IdlePad:   &Range{500, 4000},
	}
)

// Builtin returns a copy of the built-in profile with this id (registries.md §6.3).
func Builtin(id uint32) (*Profile, error) {
	switch id {
	case IDNone:
		return clone(&noneProfile), nil
	case IDDefaultUDP:
		return clone(&defaultUDPProfile), nil
	case IDDefaultTLS:
		return clone(&defaultTLSProfile), nil
	case IDConstantRateCall:
		return nil, fmt.Errorf("%w: %d", ErrReservedProfile, id)
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnknownProfile, id)
	}
}

// Parse resolves and validates the shape profile of a card address (objects.md §3.1, wire.md §9).
// A nil profile and id 0 without data mean "none".
func Parse(p *objectspb.ShapeProfile) (*Profile, error) {
	if p == nil {
		return clone(&noneProfile), nil
	}
	if p.GetId() != 0 {
		if profileHasData(p) {
			return nil, fmt.Errorf("%w: built-in id %d carries data", ErrInvalidProfile, p.GetId())
		}
		return Builtin(p.GetId())
	}
	out := &Profile{
		ID:        IDNone,
		InitLen:   profileRange(p.GetInitLen()),
		Pad:       profileRange(p.GetPad()),
		Buckets:   append([]uint32(nil), p.GetBuckets()...),
		PadBudget: profileRange(p.GetPadBudget()),
		Keepalive: Range{defaultKeepaliveMin, defaultKeepaliveMax},
		IdlePad:   profileRange(p.GetIdlePadMs()),
	}
	if j := p.GetJunk(); j != nil {
		out.Junk = &Junk{
			Count: rangeOf(j.GetCount()),
			Len:   rangeOf(j.GetLen()),
			Gap:   rangeOf(j.GetGapMs()),
		}
	}
	for _, d := range p.GetDecoys() {
		if d == nil {
			return nil, fmt.Errorf("%w: nil decoy", ErrInvalidProfile)
		}
		out.Decoys = append(out.Decoys, Decoy{
			Kind:     d.GetKind(),
			Template: append([]byte(nil), d.GetTemplate()...),
			QName:    d.GetQname(),
		})
	}
	if p.GetKeepaliveS() != nil {
		out.Keepalive = rangeOf(p.GetKeepaliveS())
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// Validate checks a profile against the limits in doc.go.
func (p *Profile) Validate() error {
	if p.InitLen != nil {
		if err := checkRange("init_len", *p.InitLen, minInitLen, maxInitLen); err != nil {
			return err
		}
	}
	if p.Pad != nil {
		if err := checkRange("pad", *p.Pad, 0, maxPad); err != nil {
			return err
		}
	}
	if len(p.Buckets) > maxBuckets {
		return fmt.Errorf("%w: %d buckets, at most %d", ErrInvalidProfile, len(p.Buckets), maxBuckets)
	}
	for i, b := range p.Buckets {
		if b < 1 || b > maxPad {
			return fmt.Errorf("%w: bucket %d is %d", ErrInvalidProfile, i, b)
		}
		if i > 0 && p.Buckets[i-1] >= b {
			return fmt.Errorf("%w: buckets not strictly ascending at %d", ErrInvalidProfile, i)
		}
	}
	if p.PadBudget != nil {
		if err := checkRange("pad_budget", *p.PadBudget, 0, ^uint32(0)); err != nil {
			return err
		}
	}
	if p.Junk != nil {
		if err := checkRange("junk.count", p.Junk.Count, 0, maxJunkCount); err != nil {
			return err
		}
		if err := checkRange("junk.len", p.Junk.Len, 1, maxJunkLen); err != nil {
			return err
		}
		if err := checkRange("junk.gap_ms", p.Junk.Gap, 0, maxJunkGapMs); err != nil {
			return err
		}
	}
	if len(p.Decoys) > maxDecoys {
		return fmt.Errorf("%w: %d decoys, at most %d", ErrInvalidProfile, len(p.Decoys), maxDecoys)
	}
	for i, d := range p.Decoys {
		if err := d.validate(); err != nil {
			return fmt.Errorf("%w: decoy %d: %v", ErrInvalidProfile, i, err)
		}
	}
	if p.Keepalive != (Range{}) {
		if err := checkRange("keepalive_s", p.Keepalive, minKeepaliveS, maxKeepaliveS); err != nil {
			return err
		}
	}
	if p.IdlePad != nil {
		if err := checkRange("idle_pad_ms", *p.IdlePad, 1, ^uint32(0)); err != nil {
			return err
		}
	}
	return nil
}

func (d Decoy) validate() error {
	switch d.Kind {
	case DecoyRaw:
		if len(d.Template) == 0 {
			return errors.New("raw decoy without a template")
		}
		if len(d.Template) > maxDecoyBytes {
			return fmt.Errorf("raw template is %d bytes, at most %d", len(d.Template), maxDecoyBytes)
		}
		if d.QName != "" {
			return errors.New("raw decoy with a query name")
		}
	case DecoyDNS:
		if d.QName == "" {
			return errors.New("DNS decoy without a query name")
		}
		if len(d.Template) != 0 {
			return errors.New("DNS decoy with a template")
		}
		if !validQName(d.QName) {
			return fmt.Errorf("DNS decoy query name %q is not a fully qualified name", d.QName)
		}
		if _, err := dnsmessage.NewName(d.QName); err != nil {
			return fmt.Errorf("DNS decoy query name %q: %v", d.QName, err)
		}
	case DecoyQUICInitial:
		if len(d.Template) != 0 || d.QName != "" {
			return errors.New("QUIC-shaped decoy with data")
		}
	default:
		return fmt.Errorf("unknown kind %d", d.Kind)
	}
	return nil
}

func validQName(s string) bool {
	if !strings.HasSuffix(s, ".") || len(s) > 255 {
		return false
	}
	labels := strings.Split(strings.TrimSuffix(s, "."), ".")
	if len(labels) == 0 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
	}
	return true
}

func checkRange(name string, r Range, min, max uint32) error {
	if r.Min > r.Max {
		return fmt.Errorf("%w: %s min %d above max %d", ErrInvalidProfile, name, r.Min, r.Max)
	}
	if r.Min < min || r.Max > max {
		return fmt.Errorf("%w: %s [%d, %d] outside [%d, %d]", ErrInvalidProfile, name, r.Min, r.Max, min, max)
	}
	return nil
}

func profileHasData(p *objectspb.ShapeProfile) bool {
	return p.GetInitLen() != nil || p.GetPad() != nil || len(p.GetBuckets()) != 0 ||
		p.GetPadBudget() != nil || p.GetJunk() != nil || len(p.GetDecoys()) != 0 ||
		p.GetKeepaliveS() != nil || p.GetIdlePadMs() != nil
}

func profileRange(r *objectspb.Range) *Range {
	if r == nil {
		return nil
	}
	v := rangeOf(r)
	return &v
}

func rangeOf(r *objectspb.Range) Range {
	if r == nil {
		return Range{}
	}
	return Range{Min: r.GetMin(), Max: r.GetMax()}
}

func clone(p *Profile) *Profile {
	out := *p
	if p.InitLen != nil {
		v := *p.InitLen
		out.InitLen = &v
	}
	if p.Pad != nil {
		v := *p.Pad
		out.Pad = &v
	}
	if p.PadBudget != nil {
		v := *p.PadBudget
		out.PadBudget = &v
	}
	if p.IdlePad != nil {
		v := *p.IdlePad
		out.IdlePad = &v
	}
	if p.Junk != nil {
		v := *p.Junk
		out.Junk = &v
	}
	out.Buckets = append([]uint32(nil), p.Buckets...)
	if p.Decoys != nil {
		out.Decoys = make([]Decoy, len(p.Decoys))
		for i, d := range p.Decoys {
			out.Decoys[i] = Decoy{Kind: d.Kind, Template: append([]byte(nil), d.Template...), QName: d.QName}
		}
	}
	return &out
}
