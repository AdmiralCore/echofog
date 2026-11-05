package exit

import (
	"encoding/binary"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// Limits of the EXIT_UDP datagram format (services.md §7).
const (
	// MaxFragments is the largest frag_count a sender may use.
	MaxFragments = 8
	// MaxIncomplete is how many incomplete packets one UDP session keeps before dropping new
	// fragments.
	MaxIncomplete = 64
	// FragmentTTL is how long an incomplete packet waits for its missing fragments.
	FragmentTTL = 2 * time.Second
)

// Fragment is one EXIT_UDP datagram:
// session(varint) ‖ pkt(u16) ‖ frag_idx(u8) ‖ frag_count(u8) ‖ [addr_len(u8) ‖ addr] ‖ data.
// The address part is present only in fragment 0.
type Fragment struct {
	Session uint64
	Packet  uint16
	Index   uint8
	Count   uint8
	Addr    string
	Data    []byte
}

// Append encodes f. addr_len counts the whole address part including its own byte, as the
// normative EXIT_UDP vector in frames.json shows: 0x0e for the 13-byte "192.0.2.53:53".
func (f Fragment) Append(b []byte) []byte {
	b = frame.AppendVarint(b, f.Session)
	b = binary.BigEndian.AppendUint16(b, f.Packet)
	b = append(b, f.Index, f.Count)
	if f.Index == 0 {
		b = append(b, byte(len(f.Addr)+1))
		b = append(b, f.Addr...)
	}
	return append(b, f.Data...)
}

// ParseFragment decodes an EXIT_UDP payload. It is strict: fragment counts outside 1..8, an index
// past the count, an address outside fragment 0, and a malformed address are all malformed.
func ParseFragment(p []byte) (Fragment, error) {
	var f Fragment
	session, n, err := frame.ParseVarint(p)
	if err != nil {
		return Fragment{}, fmt.Errorf("%w: exit_udp session: %v", frame.ErrMalformed, err)
	}
	p = p[n:]
	if len(p) < 2+1+1 {
		return Fragment{}, fmt.Errorf("%w: exit_udp header", frame.ErrMalformed)
	}
	f.Session = session
	f.Packet = binary.BigEndian.Uint16(p)
	f.Index, f.Count = p[2], p[3]
	p = p[4:]
	if f.Count < 1 || f.Count > MaxFragments || f.Index >= f.Count {
		return Fragment{}, fmt.Errorf("%w: exit_udp fragment %d/%d", frame.ErrMalformed, f.Index, f.Count)
	}
	if f.Index == 0 {
		if len(p) < 1 {
			return Fragment{}, fmt.Errorf("%w: exit_udp address length", frame.ErrMalformed)
		}
		n := int(p[0])
		if n < 2 || len(p) < n {
			return Fragment{}, fmt.Errorf("%w: exit_udp address", frame.ErrMalformed)
		}
		if !utf8.Valid(p[1:n]) {
			return Fragment{}, fmt.Errorf("%w: exit_udp address is not UTF-8", frame.ErrMalformed)
		}
		f.Addr = string(p[1:n])
		p = p[n:]
	}
	f.Data = append([]byte(nil), p...)
	if len(f.Data) > maxUDPData {
		return Fragment{}, fmt.Errorf("%w: exit_udp data too long", frame.ErrMalformed)
	}
	return f, nil
}

// reassembler collects the fragments of one UDP session.
type reassembler struct {
	now     func() time.Time
	pending map[uint16]*partialPacket
}

type partialPacket struct {
	at    time.Time
	addr  string
	count uint8
	parts [][]byte
	seen  []bool
	got   int
}

func newReassembler(now func() time.Time) *reassembler {
	return &reassembler{now: now, pending: map[uint16]*partialPacket{}}
}

// feed adds one fragment. It returns the address and payload once every fragment of a packet has
// arrived. Expired packets are dropped first; a packet older than FragmentTTL never completes.
func (r *reassembler) feed(f Fragment) (addr string, data []byte, ok bool) {
	r.expire()
	if f.Count < 1 || f.Count > MaxFragments || f.Index >= f.Count {
		return "", nil, false
	}
	now := r.now()
	p := r.pending[f.Packet]
	if p == nil {
		if len(r.pending) >= MaxIncomplete {
			return "", nil, false
		}
		p = &partialPacket{at: now, count: f.Count, parts: make([][]byte, f.Count), seen: make([]bool, f.Count)}
		r.pending[f.Packet] = p
	}
	if p.count != f.Count {
		// A sender that changes the count mid-packet is broken; drop the packet.
		delete(r.pending, f.Packet)
		return "", nil, false
	}
	if p.seen[f.Index] {
		return "", nil, false
	}
	p.seen[f.Index] = true
	p.parts[f.Index] = f.Data
	if f.Index == 0 {
		p.addr = f.Addr
	}
	p.got++
	if p.got != int(p.count) || p.addr == "" {
		return "", nil, false
	}
	out := make([]byte, 0, p.got*16)
	for _, part := range p.parts {
		out = append(out, part...)
	}
	delete(r.pending, f.Packet)
	return p.addr, out, true
}

// expire drops the packets whose Fragments have waited longer than FragmentTTL.
func (r *reassembler) expire() {
	now := r.now()
	for pkt, p := range r.pending {
		if now.Sub(p.at) > FragmentTTL {
			delete(r.pending, pkt)
		}
	}
}

// pendingCount returns the number of incomplete packets (tests).
func (r *reassembler) pendingCount() int { return len(r.pending) }
