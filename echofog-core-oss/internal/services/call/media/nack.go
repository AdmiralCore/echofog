package media

import (
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Hop-by-hop loss recovery. Every node that sends a stream to children keeps the ciphertext
// fragments it sent for a short while. A child that finds a unit missing NACKs its parent over
// call signaling, and the parent resends from this ring, or asks its own parent if it lost the
// unit too. Relays still never decrypt: they resend the sealed fragments as they got them.

const (
	// retransmitWindow is how long sent fragments stay resendable; past reorderWindow nobody
	// waits for them any more.
	retransmitWindow = 400 * time.Millisecond
	maxRingUnits     = 8192
	// nackDelay lets ordinary reordering settle before asking; nackRetry asks once more in case
	// the NACK or the resent fragments were lost as well. Both fire well before reorderWindow,
	// after which a keyframe request is the fallback.
	nackDelay = 20 * time.Millisecond
	nackRetry = 90 * time.Millisecond
	// nackHoldoff keeps a node from asking upstream for the same unit twice in a row, e.g. when
	// several children report a unit this relay never got either.
	nackHoldoff = 50 * time.Millisecond
	// MaxNackEntries bounds one NACK; a larger gap goes straight to a keyframe request.
	MaxNackEntries = maxTrackedGap
)

// NackEntry names one lost unit of a stream and the fragments the requester already holds.
type NackEntry struct {
	Seq  uint32 `json:"seq"`
	Have uint16 `json:"have,omitempty"` // bit i set = fragment i is not needed
}

type ringKey struct {
	streamID string
	seq      uint32
}

type ringUnit struct {
	frags [MaxFragments][]byte
	count uint8
	at    time.Time
}

type ringRef struct {
	key ringKey
	at  time.Time
}

// fragRing holds recently sent fragments by (stream, seq, fragment), oldest evicted first.
type fragRing struct {
	mu    sync.Mutex
	units map[ringKey]*ringUnit
	order []ringRef
	head  int
}

func newFragRing() *fragRing {
	return &fragRing{units: make(map[ringKey]*ringUnit)}
}

// put stores one wire fragment; raw must not be modified afterwards (the outbox holds it too).
func (r *fragRing) put(streamID string, seq uint32, idx, count uint8, raw []byte, now time.Time) {
	k := ringKey{streamID, seq}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked(now)
	u := r.units[k]
	if u == nil || u.count != count {
		u = &ringUnit{count: count, at: now}
		r.units[k] = u
		r.order = append(r.order, ringRef{k, now})
	}
	u.frags[idx] = raw
}

// lookup returns the fragments of a unit that a holder of have still lacks, which fragments the
// ring holds, and whether those cover everything the requester lacks.
func (r *fragRing) lookup(streamID string, seq uint32, have uint16, now time.Time) (send [][]byte, held uint16, complete bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.units[ringKey{streamID, seq}]
	if u == nil || now.Sub(u.at) >= retransmitWindow {
		return nil, 0, false
	}
	complete = true
	for i := uint8(0); i < u.count; i++ {
		if u.frags[i] != nil {
			held |= 1 << i
		}
		if have&(1<<i) != 0 {
			continue
		}
		if u.frags[i] == nil {
			complete = false
			continue
		}
		send = append(send, u.frags[i])
	}
	return send, held, complete
}

func (r *fragRing) evictLocked(now time.Time) {
	for r.head < len(r.order) {
		ref := r.order[r.head]
		if now.Sub(ref.at) < retransmitWindow && len(r.order)-r.head <= maxRingUnits {
			break
		}
		if u := r.units[ref.key]; u != nil && u.at == ref.at {
			delete(r.units, ref.key)
		}
		r.order[r.head] = ringRef{}
		r.head++
	}
	if r.head > 1024 && r.head*2 > len(r.order) {
		r.order = append(r.order[:0], r.order[r.head:]...)
		r.head = 0
	}
}

func (r *fragRing) expire(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked(now)
}

// ServeNack answers a child's NACK from the ring and returns the units this node has to ask its
// own parent for. When those arrive they are forwarded to every child as usual, the requester
// included. A NACK from a peer that is not our child on the stream is ignored.
func (d *MediaDispatcher) ServeNack(child types.PeerID, streamID string, lost []NackEntry) []NackEntry {
	if d.streamEngine == nil || d.outbox == nil || len(lost) == 0 || len(lost) > MaxNackEntries {
		return nil
	}
	isChild := false
	for _, c := range d.streamEngine.GetDownstream(streamID, d.selfID) {
		if c == child {
			isChild = true
			break
		}
	}
	if !isChild {
		return nil
	}
	now := time.Now()
	var upstream []NackEntry
	for _, e := range lost {
		frags, held, complete := d.ring.lookup(streamID, e.Seq, e.Have, now)
		for _, f := range frags {
			d.retransmitted.Add(1)
			d.outbox.Send(child, ClassVideo, net_types.TypeCallMediaVideo, f)
		}
		if !complete {
			// whatever arrives from upstream is forwarded to every child, so ask for all we lack
			upstream = append(upstream, NackEntry{Seq: e.Seq, Have: held})
		}
	}
	upstream = d.holdoff(streamID, upstream, now)
	d.nacked.Add(uint64(len(upstream)))
	return upstream
}

// nackGap asks upstream for the units of a gap that are still missing.
func (d *MediaDispatcher) nackGap(k sourceKey, gap []uint32) {
	select {
	case <-d.stop:
		return
	default:
	}
	var lost []NackEntry
	d.mu.Lock()
	m := d.missing[k]
	for _, s := range gap {
		if _, ok := m[s]; ok {
			lost = append(lost, NackEntry{Seq: s})
		}
	}
	cb := d.OnVideoNack
	d.mu.Unlock()
	if len(lost) == 0 || cb == nil {
		return
	}
	now := time.Now()
	for i := range lost {
		_, held, _ := d.ring.lookup(k.streamID, lost[i].Seq, 0, now)
		lost[i].Have = held | d.reasm.have(unitKey{k.kind, k.broadcaster, k.streamID, lost[i].Seq})
	}
	if lost = d.holdoff(k.streamID, lost, now); len(lost) > 0 {
		d.nacked.Add(uint64(len(lost)))
		cb(k.streamID, lost)
	}
}

// holdoff drops units this node asked upstream for a moment ago.
func (d *MediaDispatcher) holdoff(streamID string, lost []NackEntry, now time.Time) []NackEntry {
	if len(lost) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := lost[:0]
	for _, e := range lost {
		k := ringKey{streamID, e.Seq}
		if now.Sub(d.nackAsked[k]) < nackHoldoff {
			continue
		}
		d.nackAsked[k] = now
		out = append(out, e)
	}
	return out
}
