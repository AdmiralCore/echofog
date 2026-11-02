package media

import (
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// replayWindowSize is how many recent sequence numbers per source are remembered.
// At 4K60 (~2.5k units/s) this covers ~400 ms of reordering.
const replayWindowSize = 1024

type windowSlot struct {
	seq  uint32
	mask uint16 // fragment windows (FEC parity): bit i set = lane i already seen
	done bool   // unit windows: a unit with this seq authenticated
	used bool
}

// replayWindow is an SRTP-style sliding window: O(1) per unit, no allocations. Sequence numbers
// travel in the clear, so a unit window remembers only units that authenticated (mark): anybody
// could otherwise take a source's next numbers first, with fragments it cannot even seal, and every
// real unit behind them would be dropped as a duplicate, muting the source for the whole subtree.
type replayWindow struct {
	highest  uint32
	started  bool
	lastSeen time.Time
	slots    [replayWindowSize]windowSlot
}

// tooOld reports whether seq fell out of the window.
func (w *replayWindow) tooOld(seq uint32) bool {
	// serial arithmetic handles uint32 wrap-around
	diff := int32(seq - w.highest)
	return w.started && diff <= 0 && -diff >= replayWindowSize
}

// seen reports whether a unit with this seq authenticated already, or is too old to take.
func (w *replayWindow) seen(seq uint32) bool {
	if w.tooOld(seq) {
		return true
	}
	slot := &w.slots[seq%replayWindowSize]
	return slot.used && slot.seq == seq && slot.done
}

// mark records a unit that authenticated and moves the window; false if a copy of it came first.
func (w *replayWindow) mark(seq uint32, now time.Time) bool {
	if w.seen(seq) {
		return false
	}
	w.lastSeen = now
	slot := &w.slots[seq%replayWindowSize]
	*slot = windowSlot{seq: seq, done: true, used: true}
	if !w.started || int32(seq-w.highest) > 0 {
		w.started, w.highest = true, seq
	}
	return true
}

// checkFragment is for what cannot be authenticated on its own (FEC parity, keyed by its lane):
// true if the fragment is new, and it is recorded. It never moves the window, so a forgery costs
// at most the parity it shadows.
func (w *replayWindow) checkFragment(seq uint32, frag uint8, now time.Time) bool {
	w.lastSeen = now
	slot := &w.slots[seq%replayWindowSize]
	if !slot.used || slot.seq != seq {
		*slot = windowSlot{seq: seq, used: true}
	}
	bit := uint16(1) << frag
	if slot.mask&bit != 0 {
		return false
	}
	slot.mask |= bit
	return true
}

type sourceKey struct {
	kind        FrameKind
	broadcaster types.PeerID
	streamID    string
}

// dedupTable holds one replay window per (kind, broadcaster, stream).
type dedupTable struct {
	mu      sync.Mutex
	windows map[sourceKey]*replayWindow
}

func newDedupTable() *dedupTable {
	return &dedupTable{windows: make(map[sourceKey]*replayWindow)}
}

func (t *dedupTable) windowLocked(p *MediaPacket) *replayWindow {
	k := sourceKey{p.Kind, p.Broadcaster, p.StreamID}
	w := t.windows[k]
	if w == nil {
		w = &replayWindow{}
		t.windows[k] = w
	}
	return w
}

// seen reports whether the packet's unit authenticated already (or is too old): its fragments
// need no work then. It records nothing.
func (t *dedupTable) seen(p *MediaPacket) bool {
	k := sourceKey{p.Kind, p.Broadcaster, p.StreamID}
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.windows[k]
	return w != nil && w.seen(p.Seq)
}

// mark records an authenticated unit of the packet's source; false if a copy of it came first.
func (t *dedupTable) mark(p *MediaPacket, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.windowLocked(p).mark(p.Seq, now)
}

// acceptFragment records a fragment that cannot be authenticated by itself (FEC parity).
func (t *dedupTable) acceptFragment(p *MediaPacket, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.windowLocked(p).checkFragment(p.Seq, p.FragIdx, now)
}

// gc drops windows of sources that went silent.
func (t *dedupTable) gc(now time.Time, idle time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, w := range t.windows {
		if now.Sub(w.lastSeen) > idle {
			delete(t.windows, k)
		}
	}
}
