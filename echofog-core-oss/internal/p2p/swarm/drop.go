package swarm

import (
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// dropLogEvery is how often at most a dropped frame is logged; the rest are only counted.
const dropLogEvery = 30 * time.Second

// dropState: frame types this node runs no service for, and how many arrived anyway.
type dropState struct {
	types   atomic.Pointer[map[net_types.MessageType]struct{}] // replaced whole, read without a lock
	count   atomic.Uint64
	lastLog atomic.Int64 // unix nanoseconds of the last log line
}

// DropFrameTypes makes the swarm discard inbound frames and streams of these types as they arrive,
// before any decoding, signature check, handler or disk write. A server-role node uses it for the
// chat, call, file and pre-key bundle traffic it runs no service for. It adds to earlier calls.
func (s *Swarm) DropFrameTypes(ts ...net_types.MessageType) {
	for {
		old := s.drop.types.Load()
		next := make(map[net_types.MessageType]struct{}, len(ts))
		if old != nil {
			for t := range *old {
				next[t] = struct{}{}
			}
		}
		for _, t := range ts {
			next[t] = struct{}{}
		}
		if s.drop.types.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Drops reports whether inbound frames of this type are discarded on arrival.
func (s *Swarm) Drops(t net_types.MessageType) bool {
	if s == nil {
		return false
	}
	m := s.drop.types.Load()
	if m == nil {
		return false
	}
	_, ok := (*m)[t]
	return ok
}

// DroppedFrames is how many inbound frames and streams were discarded by type so far.
func (s *Swarm) DroppedFrames() uint64 { return s.drop.count.Load() }

// dropped counts a discarded frame and logs it at debug level, at most once per dropLogEvery.
func (s *Swarm) dropped(t net_types.MessageType, from types.PeerID) {
	n := s.drop.count.Add(1)
	now := time.Now().UnixNano()
	last := s.drop.lastLog.Load()
	if now-last < int64(dropLogEvery) || !s.drop.lastLog.CompareAndSwap(last, now) {
		return
	}
	s.log().Debug("Swarm: dropped a frame of a type this node does not serve", "type", t, "peer", from.String(), "dropped_total", n)
}
