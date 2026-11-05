package exit

import (
	"time"
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

// Append encodes f.
func (f Fragment) Append(b []byte) []byte { return b }

// ParseFragment decodes an EXIT_UDP payload.
func ParseFragment(p []byte) (Fragment, error) { return Fragment{}, nil }

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
	got   int
}

func newReassembler(now func() time.Time) *reassembler {
	return &reassembler{now: now, pending: map[uint16]*partialPacket{}}
}

// feed adds one fragment. It returns the address and payload once every fragment of a packet has
// arrived. Expired packets are dropped first; a packet older than FragmentTTL never completes.
func (r *reassembler) feed(f Fragment) (addr string, data []byte, ok bool) {
	return "", nil, false
}

// pendingCount returns the number of incomplete packets (tests).
func (r *reassembler) pendingCount() int { return 0 }
