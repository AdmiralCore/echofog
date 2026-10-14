package media

import (
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	reassemblyTimeout    = time.Second
	maxPendingReassembly = 4096
)

type unitKey struct {
	kind        FrameKind
	broadcaster types.PeerID
	streamID    string
	seq         uint32
}

type pendingUnit struct {
	hdr     MediaPacket
	frags   [MaxFragments][]byte
	have    uint16
	created time.Time
}

// reassembler joins fragments of a unit; only receivers that consume the media need it,
// relays forward fragments untouched.
type reassembler struct {
	mu      sync.Mutex
	pending map[unitKey]*pendingUnit
}

func newReassembler() *reassembler {
	return &reassembler{pending: make(map[unitKey]*pendingUnit)}
}

// add returns the full ciphertext once every fragment of the unit has arrived.
func (r *reassembler) add(p *MediaPacket, now time.Time) ([]byte, bool) {
	if p.FragCount == 1 {
		return p.Payload, true
	}
	k := unitKey{p.Kind, p.Broadcaster, p.StreamID, p.Seq}

	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.pending[k]
	if u == nil {
		if len(r.pending) >= maxPendingReassembly {
			r.expireLocked(now, 0)
		}
		u = &pendingUnit{hdr: *p, created: now}
		u.hdr.Payload = nil
		r.pending[k] = u
	}
	if p.FragCount != u.hdr.FragCount {
		return nil, false
	}
	u.frags[p.FragIdx] = append([]byte(nil), p.Payload...)
	u.have |= 1 << p.FragIdx
	if u.have != (uint16(1)<<p.FragCount)-1 {
		return nil, false
	}
	delete(r.pending, k)
	size := 0
	for i := 0; i < int(p.FragCount); i++ {
		size += len(u.frags[i])
	}
	out := make([]byte, 0, size)
	for i := 0; i < int(p.FragCount); i++ {
		out = append(out, u.frags[i]...)
	}
	return out, true
}

// expire drops incomplete units older than the timeout and reports how many were lost.
func (r *reassembler) expire(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expireLocked(now, reassemblyTimeout)
}

func (r *reassembler) expireLocked(now time.Time, maxAge time.Duration) int {
	n := 0
	for k, u := range r.pending {
		if now.Sub(u.created) >= maxAge {
			delete(r.pending, k)
			n++
		}
	}
	return n
}
