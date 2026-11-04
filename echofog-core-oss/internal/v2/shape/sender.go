package shape

import (
	"encoding/binary"
	mrand "math/rand/v2"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// quicInitialDecoyLen is the size of a QUIC client Initial: datagrams below 1200 bytes are not
// Initials (RFC 9000 §14.1), so a shorter decoy would be recognisable by length alone.
const quicInitialDecoyLen = 1200

// Datagram is one datagram to send before the first noise3 Init, preceded by its gap.
type Datagram struct {
	Gap  time.Duration
	Data []byte
}

// Sender draws the shape of one stream direction. It owns the padding budget of that direction and
// is not safe for concurrent use. A nil rand uses the package-level math/rand/v2 generator; tests
// pass a seeded *rand.Rand for reproducible draws.
type Sender struct {
	p        *Profile
	r        *mrand.Rand
	lowPower bool
	budget   int64 // remaining bytes of the padding window; -1 = unlimited
}

// NewSender returns the generator for one stream direction of a profile. lowPower is the platform
// `low_power` flag (platforms.md §2): it disables idle padding. p may be nil, which means "none".
func NewSender(p *Profile, r *mrand.Rand, lowPower bool) *Sender {
	s := &Sender{p: p, r: r, lowPower: lowPower, budget: -1}
	if p != nil && p.PadBudget != nil {
		s.budget = int64(s.uniform(*p.PadBudget))
	}
	return s
}

// NextInitLen returns the drawn total length of a noise3 Init datagram, or 0 when the profile does
// not set init_len: the sender then uses the suite minimum of wire.md §2.4 (max(drawn, 71+inner)).
func (s *Sender) NextInitLen() int {
	if s.p == nil || s.p.InitLen == nil {
		return 0
	}
	return s.uniform(*s.p.InitLen)
}

// PadFor returns the number of padding bytes to add to a packet or TLS record that is n bytes long.
// With buckets the record is rounded up to the next bucket, otherwise pad is drawn uniformly. On
// carriers with a pad_budget the result also spends the budget of this direction. The window counts
// the direction's total bytes including the record itself: padding never crosses its end, and a
// record that does not fit in the remaining window is not padded.
func (s *Sender) PadFor(n int) int {
	if n < 0 || s.p == nil {
		return 0
	}
	pad := 0
	switch {
	case len(s.p.Buckets) > 0:
		for _, b := range s.p.Buckets {
			if int(b) >= n {
				pad = int(b) - n
				break
			}
		}
	case s.p.Pad != nil:
		pad = s.uniform(*s.p.Pad)
	}
	if s.budget >= 0 {
		remaining := s.budget - int64(n)
		if remaining <= 0 {
			s.budget = 0
			return 0
		}
		if int64(pad) > remaining {
			pad = int(remaining)
		}
		s.budget = remaining - int64(pad)
	}
	return pad
}

// Junk returns the junk datagrams to send before the first Init (wire.md §2.7, §9), each with the
// gap that precedes it.
func (s *Sender) Junk() []Datagram {
	if s.p == nil || s.p.Junk == nil {
		return nil
	}
	count := s.uniform(s.p.Junk.Count)
	if count == 0 {
		return nil
	}
	out := make([]Datagram, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, Datagram{
			Gap:  time.Duration(s.uniform(s.p.Junk.Gap)) * time.Millisecond,
			Data: s.bytes(s.uniform(s.p.Junk.Len)),
		})
	}
	return out
}

// Decoys returns the decoy datagrams of the profile in order (wire.md §2.7, §9). They carry no gap:
// the specification gives a gap only to junk.
func (s *Sender) Decoys() []Datagram {
	if s.p == nil || len(s.p.Decoys) == 0 {
		return nil
	}
	out := make([]Datagram, 0, len(s.p.Decoys))
	for _, d := range s.p.Decoys {
		var b []byte
		switch d.Kind {
		case DecoyRaw:
			b = s.template(d.Template)
		case DecoyDNS:
			b = s.dnsQuery(d.QName)
		case DecoyQUICInitial:
			b = s.quicInitial()
		}
		if b != nil {
			out = append(out, Datagram{Data: b})
		}
	}
	return out
}

// Keepalive returns the interval after which an idle path sends PING (wire.md §7.1, §9). A profile
// without keepalive_s uses the §7.1 range [12, 24] s.
func (s *Sender) Keepalive() time.Duration {
	r := Range{defaultKeepaliveMin, defaultKeepaliveMax}
	if s.p != nil && s.p.Keepalive != (Range{}) {
		r = s.p.Keepalive
	}
	return time.Duration(s.uniform(r)) * time.Second
}

// IdlePad returns the gap between padding-only packets when the `idle-padding` feature is in use
// (wire.md §9, §10). It reports false when the profile has no idle_pad_ms or the platform runs in
// `low_power`, which offers no idle padding (platforms.md §2).
func (s *Sender) IdlePad() (time.Duration, bool) {
	if s.lowPower || s.p == nil || s.p.IdlePad == nil || s.p.IdlePad.Max == 0 {
		return 0, false
	}
	return time.Duration(s.uniform(*s.p.IdlePad)) * time.Millisecond, true
}

// uniform draws an inclusive value from r.
func (s *Sender) uniform(r Range) int {
	span := int64(r.Max) - int64(r.Min) + 1
	if span <= 1 {
		return int(r.Min)
	}
	return int(int64(r.Min) + s.int64N(span))
}

func (s *Sender) int64N(n int64) int64 {
	if s.r != nil {
		return s.r.Int64N(n)
	}
	return mrand.Int64N(n)
}

func (s *Sender) uint64() uint64 {
	if s.r != nil {
		return s.r.Uint64()
	}
	return mrand.Uint64()
}

// bytes returns n random bytes.
func (s *Sender) bytes(n int) []byte {
	b := make([]byte, n)
	s.fill(b)
	return b
}

// fill overwrites b with random bytes.
func (s *Sender) fill(b []byte) {
	for len(b) >= 8 {
		binary.LittleEndian.PutUint64(b, s.uint64())
		b = b[8:]
	}
	if len(b) > 0 {
		v := s.uint64()
		for i := range b {
			b[i] = byte(v >> (8 * i))
		}
	}
}

// template copies a raw decoy and replaces every maximal run of 0xff with random bytes.
func (s *Sender) template(tmpl []byte) []byte {
	out := append([]byte(nil), tmpl...)
	for i := 0; i < len(out); {
		if out[i] != 0xff {
			i++
			continue
		}
		j := i
		for j < len(out) && out[j] == 0xff {
			j++
		}
		s.fill(out[i:j])
		i = j
	}
	return out
}

// dnsQuery builds a DNS query for the decoy (wire.md §2.7). The name is validated when the profile
// is read; a name that does not pack is skipped by Decoys.
func (s *Sender) dnsQuery(qname string) []byte {
	name, err := dnsmessage.NewName(qname)
	if err != nil {
		return nil
	}
	m := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:               uint16(s.int64N(1 << 16)),
			RecursionDesired: true,
		},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
		}},
	}
	b, err := m.Pack()
	if err != nil {
		return nil
	}
	return b
}

// quicInitial builds a 1200-byte QUIC v1 long-header datagram (wire.md §2.7): a random connection
// ID, no source ID, no token, and a random payload behind the length field.
func (s *Sender) quicInitial() []byte {
	b := make([]byte, quicInitialDecoyLen)
	b[0] = 0xc0 | byte(s.int64N(0x10)) // long header, fixed bit, type Initial
	binary.BigEndian.PutUint32(b[1:5], 1)
	b[5] = 8 // destination connection ID length
	s.fill(b[6:14])
	b[14] = 0 // source connection ID length
	b[15] = 0 // token length
	// Length is a two-byte QUIC varint over the packet number and payload that follow.
	binary.BigEndian.PutUint16(b[16:18], 0x4000|uint16(quicInitialDecoyLen-18))
	s.fill(b[18:])
	return b
}
