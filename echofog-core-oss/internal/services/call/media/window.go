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

// replayWindow is an SRTP-style sliding window: O(1) per fragment, no allocations. Fragments carry
// their sequence number in the clear and are checked here before anything authenticates them, so
// only a unit that decrypted moves the window (commit): a forged fragment with a far-ahead number
// would otherwise push the window past the real stream and mute its source for everyone downstream.
type replayWindow struct {
	highest  uint32
	started  bool
	lastSeen time.Time
	slots    [replayWindowSize]windowSlot
}

// check returns true if the fragment is new and records it; false for duplicates or stale fragments.
func (w *replayWindow) check(seq uint32, frag uint8, now time.Time) bool {
	w.lastSeen = now
	if w.started {
		// serial arithmetic handles uint32 wrap-around
		if diff := int32(seq - w.highest); diff <= 0 && -diff >= replayWindowSize {
			return false // too old
		}
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

// commit records an authenticated unit: only these move the window forward.
func (w *replayWindow) commit(seq uint32) {
	if !w.started || int32(seq-w.highest) > 0 {
		w.started, w.highest = true, seq
	}
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

// commit moves the window of the packet's source to its sequence number, once the unit decrypted.
func (t *dedupTable) commit(p *MediaPacket) {
	k := sourceKey{p.Kind, p.Broadcaster, p.StreamID}
	t.mu.Lock()
	defer t.mu.Unlock()
	if w := t.windows[k]; w != nil {
		w.commit(p.Seq)
	}
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
