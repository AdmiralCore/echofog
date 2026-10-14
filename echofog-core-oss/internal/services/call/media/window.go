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
	mask uint16 // bit i set = fragment i already seen
	used bool
}

// replayWindow is an SRTP-style sliding window: O(1) per fragment, no allocations.
type replayWindow struct {
	highest  uint32
	started  bool
	lastSeen time.Time
	slots    [replayWindowSize]windowSlot
}

// check returns true if the fragment is new and records it; false for duplicates or stale fragments.
func (w *replayWindow) check(seq uint32, frag uint8, now time.Time) bool {
	w.lastSeen = now
	if !w.started {
		w.started, w.highest = true, seq
	}
	diff := int32(seq - w.highest) // serial arithmetic handles uint32 wrap-around
	if diff > 0 {
		w.highest = seq
	} else if -diff >= replayWindowSize {
		return false
	}
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

func (t *dedupTable) accept(p *MediaPacket, now time.Time) bool {
	k := sourceKey{p.Kind, p.Broadcaster, p.StreamID}
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.windows[k]
	if w == nil {
		w = &replayWindow{}
		t.windows[k] = w
	}
	return w.check(p.Seq, p.FragIdx, now)
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
