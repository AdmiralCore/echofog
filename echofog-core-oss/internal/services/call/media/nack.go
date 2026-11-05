package media

import (
	"log/slog"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Hop-by-hop loss recovery. Every node that sends a stream to children keeps the ciphertext
// fragments it sent for a short while. A child that finds a unit missing NACKs its parent over
// call signaling, and the parent resends from this ring, or asks its own parent if it lost the
// unit too. The ring holds only units that authenticated, as the sealed fragments they came in.

const (
	// retransmitWindow is how long sent fragments stay resendable; past maxLossWait nobody
	// waits for them any more.
	retransmitWindow = time.Second
	maxRingUnits     = 8192
	// nackDelay lets ordinary reordering settle before asking; nackRetry asks again in case the
	// NACK or the resent fragments were lost as well. Both fire before reorderWindow, after which a
	// keyframe request is the fallback; on a slow path both grow with its round trip.
	nackDelay = 20 * time.Millisecond
	nackRetry = 90 * time.Millisecond
	// nackHoldoff keeps a node from asking upstream for the same unit twice in a row, e.g. when
	// several children report a unit this relay never got either.
	nackHoldoff = 50 * time.Millisecond
	// MaxNackEntries bounds one NACK; a larger gap goes straight to a keyframe request.
	MaxNackEntries = maxTrackedGap

	// A NACK of a kilobyte could name 64 units of 16 fragments each, over a megabyte, and the same
	// again a moment later: a child could make its parent pour resends at whoever it liked. What one
	// NACK resends is bounded, a unit named twice is resent once, and each child has a budget that
	// refills at nackRate up to nackBurst. Honest loss stays far below: 10% of a 4K stream is ~450
	// KB a second, and one NACK of real losses rarely asks for more than a fragment per unit.
	nackMaxBytes = 96 << 10
	nackRate     = 1 << 20
	nackBurst    = 256 << 10
)

// resendBudget is what a child may still pull from this node's ring, in bytes.
type resendBudget struct {
	tokens float64
	at     time.Time
}

// takeResendLocked spends size bytes of a child's budget if it has them; d.mu must be held.
func (d *MediaDispatcher) takeResendLocked(child types.PeerID, size int, now time.Time) bool {
	if d.resend == nil {
		d.resend = make(map[types.PeerID]*resendBudget)
	}
	b := d.resend[child]
	if b == nil {
		b = &resendBudget{tokens: nackBurst, at: now}
		d.resend[child] = b
	}
	b.tokens = min(nackBurst, b.tokens+now.Sub(b.at).Seconds()*nackRate)
	b.at = now
	if float64(size) > b.tokens {
		return false
	}
	b.tokens -= float64(size)
	return true
}

// NackEntry names one lost unit of a stream and the fragments the requester already holds.
type NackEntry struct {
	Seq  uint32 `json:"seq"`
	Have uint16 `json:"have,omitempty"` // bit i set = fragment i is not needed
}

// nackAsk is when this node first and last asked upstream for a unit; the first one dates the
// answer, the last one holds off repeats.
type nackAsk struct {
	first, last time.Time
}

type ringKey struct {
	streamID string
	seq      uint32
}

type ringUnit struct {
	frags [MaxFragments][]byte
	count uint8
	at    time.Time
	layer ringLayer
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
func (r *fragRing) put(streamID string, seq uint32, idx, count uint8, raw []byte, layer ringLayer, now time.Time) {
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
	u.frags[idx], u.layer = raw, layer
}

// layerOf is the layer of a unit the ring holds.
func (r *fragRing) layerOf(streamID string, seq uint32) (ringLayer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.units[ringKey{streamID, seq}]
	if u == nil {
		return ringLayer{}, false
	}
	return u.layer, true
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
	if d.streamEngine == nil || d.outbox == nil || len(lost) == 0 {
		return nil
	}
	if len(lost) > MaxNackEntries {
		d.lim.Log(d.log, slog.LevelDebug, "nack-big:"+streamID, "Ignored oversized NACK",
			"stream", streamID, "from", calllog.Peer(child), "entries", len(lost))
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
		// usually a NACK that crossed a tree update; persistent ones mean the trees disagree
		d.lim.Log(d.log, slog.LevelDebug, "nack-stranger:"+streamID+":"+child.String(), "Ignored NACK from a peer that is not our child on the stream",
			"stream", streamID, "from", calllog.Peer(child))
		return nil
	}
	now := time.Now()
	var upstream []NackEntry
	sent, volume, capped := 0, 0, false
	named := make(map[uint32]bool, len(lost))
	for _, e := range lost {
		if named[e.Seq] {
			continue // a unit named twice is resent once
		}
		named[e.Seq] = true
		hop, send := d.resendCap(child, streamID, e.Seq)
		if !send {
			continue // a frame left out for this child: it was never sent, and it is not owed
		}
		frags, held, complete := d.ring.lookup(streamID, e.Seq, e.Have, now)
		if hop != 0 {
			for i, f := range frags {
				frags[i] = withHopCap(f, hop-1)
			}
		}
		size := 0
		for _, f := range frags {
			size += len(f)
		}
		if size > 0 {
			d.mu.Lock()
			ok := volume+size <= nackMaxBytes && d.takeResendLocked(child, size, now)
			d.mu.Unlock()
			if !ok {
				capped = true
				break // the rest waits for the child's next NACK, or becomes a keyframe request
			}
			volume += size
		}
		for _, f := range frags {
			d.retransmitted.Add(1)
			sent++
			d.outbox.Send(child, ClassVideo, transport.MediaVideo, f)
		}
		if !complete {
			// whatever arrives from upstream is forwarded to every child, so ask for all we lack
			upstream = append(upstream, NackEntry{Seq: e.Seq, Have: held})
		}
	}
	if capped {
		d.lim.Log(d.log, slog.LevelDebug, "nack-capped:"+streamID+":"+child.String(), "A child asked for more resends than its budget",
			"stream", streamID, "child", calllog.Peer(child), "resent_bytes", volume)
	}
	upstream = d.holdoff(streamID, upstream, now)
	d.nacked.Add(uint64(len(upstream)))
	d.mu.Lock()
	c := d.streamLocked(streamID)
	c.Retransmitted += uint64(sent)
	c.Nacked += uint64(len(upstream))
	d.mu.Unlock()
	if len(upstream) > 0 {
		d.lim.Log(d.log, slog.LevelDebug, "nack-up:"+streamID, "Units a child asked for are not in our ring, asking upstream",
			"stream", streamID, "child", calllog.Peer(child), "units", len(upstream))
	}
	return upstream
}

// resendCap says whether a unit the ring may hold goes to a child that asks for it again, and the
// cap it goes with (plus one; 0: none said): only a unit of a frame its layers took.
func (d *MediaDispatcher) resendCap(child types.PeerID, streamID string, seq uint32) (uint8, bool) {
	l, ok := d.ring.layerOf(streamID, seq)
	if !ok || !l.marked {
		return 0, true
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	f := d.filters[linkKey{streamID, child}]
	if !f.active() {
		return 0, true
	}
	c, known := f.capOf(l.frame)
	if !known {
		c = f.cap
	}
	if l.class > c {
		return 0, false
	}
	return min(c, l.cap) + 1, true
}

// nackGap asks upstream for the units of a gap that are still missing.
func (d *MediaDispatcher) nackGap(k sourceKey, gap []uint32) {
	select {
	case <-d.stop:
		return
	default:
	}
	var lost []NackEntry
	first := 0 // units asked for the first time: what the path lost
	d.mu.Lock()
	m := d.missing[k]
	for _, s := range gap {
		if _, ok := m[s]; ok {
			lost = append(lost, NackEntry{Seq: s})
			if _, asked := d.nackAsked[ringKey{k.streamID, s}]; !asked {
				first++
			}
		}
	}
	cb := d.OnVideoNack
	d.mu.Unlock()
	if first > 0 {
		d.rx.lost(k.streamID, first)
	}
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
		d.mu.Lock()
		d.streamLocked(k.streamID).Nacked += uint64(len(lost))
		d.mu.Unlock()
		d.lim.Log(d.log, slog.LevelDebug, "nack:"+k.streamID, "Asking parent to resend lost video units",
			"stream", k.streamID, "units", len(lost))
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
		a, asked := d.nackAsked[k]
		if asked && now.Sub(a.last) < nackHoldoff {
			continue
		}
		if !asked {
			a.first = now
		}
		a.last = now
		d.nackAsked[k] = a
		out = append(out, e)
	}
	return out
}
