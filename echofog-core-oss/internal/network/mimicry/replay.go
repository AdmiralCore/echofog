package mimicry

import (
	"sync"
	"time"
)

// replayCache remembers tokens already accepted, so a stranger who copied a legitimate client's
// ClientHello cannot reuse its token within the clock-drift window. A token is valid only for
// ReplayWindowSeconds either way, so an entry can be forgotten after twice that.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]int64 // token key -> unix expiry
	max  int
}

func newReplayCache() *replayCache {
	return &replayCache{seen: make(map[string]int64), max: 100000}
}

// seenBefore records key and returns whether it was already there. now is unix seconds.
func (r *replayCache) seenBefore(key string, now int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if exp, ok := r.seen[key]; ok && exp > now {
		return true
	}
	if len(r.seen) >= r.max {
		for k, exp := range r.seen { // drop expired first; if none, this still bounds growth
			if exp <= now {
				delete(r.seen, k)
			}
		}
		if len(r.seen) >= r.max {
			r.seen = make(map[string]int64)
		}
	}
	r.seen[key] = now + 2*ReplayWindowSeconds
	return false
}

var sharedReplayCache = newReplayCache()

func nowUnix() int64 { return time.Now().Unix() }
