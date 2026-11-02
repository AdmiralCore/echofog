package media

import (
	"sync"
	"sync/atomic"
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
	raws    [MaxFragments][]byte // the wire fragments, relayed once the unit authenticated
	have    uint16
	rebuilt uint16 // fragments rebuilt here from FEC parity, not received
	held    bool   // complete with a rebuilt fragment: waits for settle
	created time.Time
}

// reassembler joins fragments of a unit. Every node needs it: a unit is relayed only once it
// authenticated, so a relay holds the fragments of a unit until the last one came.
type reassembler struct {
	mu      sync.Mutex
	pending map[unitKey]*pendingUnit
	held    atomic.Int32 // units waiting for settle: none, nearly always
}

func newReassembler() *reassembler {
	return &reassembler{pending: make(map[unitKey]*pendingUnit)}
}

// add returns the full ciphertext and the wire fragments (raw is this one) once every fragment
// of the unit has arrived. rebuilt says raw was rebuilt here from FEC parity: such a fragment never
// takes the place of one that came, and one that comes takes its place. A unit completed with a
// rebuilt fragment stays pending until settle says whether it authenticated.
func (r *reassembler) add(p *MediaPacket, raw []byte, now time.Time, rebuilt bool) ([]byte, [][]byte, bool) {
	if p.FragCount == 1 {
		return p.Payload, [][]byte{raw}, true
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
		return nil, nil, false
	}
	bit := uint16(1) << p.FragIdx
	if rebuilt && u.have&bit != 0 {
		return nil, nil, false
	}
	u.raws[p.FragIdx] = append([]byte(nil), raw...)
	u.frags[p.FragIdx] = u.raws[p.FragIdx][len(raw)-len(p.Payload):]
	u.have |= bit
	if rebuilt {
		u.rebuilt |= bit
	} else {
		u.rebuilt &^= bit
	}
	if u.have != (uint16(1)<<p.FragCount)-1 {
		return nil, nil, false
	}
	if u.rebuilt == 0 {
		r.dropLocked(k, u)
	} else if !u.held {
		u.held = true
		r.held.Add(1)
	}
	size := 0
	for i := 0; i < int(p.FragCount); i++ {
		size += len(u.frags[i])
	}
	out := make([]byte, 0, size)
	for i := 0; i < int(p.FragCount); i++ {
		out = append(out, u.frags[i]...)
	}
	return out, append([][]byte(nil), u.raws[:p.FragCount]...), true
}

// settle closes a unit add completed with a fragment rebuilt here: gone once it authenticated (ok).
// One that did not keeps the fragments that came and waits for the rest again: the parity that
// rebuilt it was wrong (or forged), and the real fragment may still come.
func (r *reassembler) settle(p *MediaPacket, ok bool) {
	if p.FragCount == 1 || r.held.Load() == 0 {
		return
	}
	k := unitKey{p.Kind, p.Broadcaster, p.StreamID, p.Seq}
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.pending[k]
	if u == nil || !u.held {
		return
	}
	if ok {
		r.dropLocked(k, u)
		return
	}
	u.held = false
	r.held.Add(-1)
	for i := range u.raws {
		if u.rebuilt&(1<<i) != 0 {
			u.raws[i], u.frags[i] = nil, nil
		}
	}
	u.have &^= u.rebuilt
	u.rebuilt = 0
}

// have reports which fragments of a pending unit have arrived.
func (r *reassembler) have(k unitKey) uint16 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u := r.pending[k]; u != nil {
		return u.have
	}
	return 0
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
			r.dropLocked(k, u)
			n++
		}
	}
	return n
}

// dropLocked forgets a pending unit; r.mu is held.
func (r *reassembler) dropLocked(k unitKey, u *pendingUnit) {
	if u.held {
		r.held.Add(-1)
	}
	delete(r.pending, k)
}
