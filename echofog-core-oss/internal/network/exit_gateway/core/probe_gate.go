package core

import (
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// probeClientWindow is how long after its last tunnel open a client may ask this exit for a
// download speed burst. A client's speed check opens test tunnels first, so it is always inside.
const probeClientWindow = 2 * time.Minute

// maxProbeClients bounds the table of recent clients; the oldest entry makes room.
const maxProbeClients = 4096

// recentClients remembers when each peer this exit serves last opened a tunnel.
type recentClients struct {
	mu   sync.Mutex
	seen map[types.PeerID]time.Time
}

func (r *recentClients) note(peer types.PeerID, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[types.PeerID]time.Time)
	}
	if _, ok := r.seen[peer]; !ok && len(r.seen) >= maxProbeClients {
		var oldest types.PeerID
		var at time.Time
		for id, t := range r.seen {
			if now.Sub(t) >= probeClientWindow {
				delete(r.seen, id)
			} else if at.IsZero() || t.Before(at) {
				oldest, at = id, t
			}
		}
		if len(r.seen) >= maxProbeClients {
			delete(r.seen, oldest)
		}
	}
	r.seen[peer] = now
}

func (r *recentClients) recent(peer types.PeerID, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.seen[peer]
	return ok && now.Sub(t) < probeClientWindow
}

// ProbeClient is the gate for answering download speed probes (bwprobe.Gate): only a peer this exit
// serves, that opened a tunnel through it in the last two minutes, gets a burst. addr is the
// address the per-address interval counts it under ("" for loopback and LAN peers).
func (e *ExitService) ProbeClient(peer types.PeerID) (addr string, ok bool) {
	if !e.serves(peer) || !e.clients.recent(peer, time.Now()) {
		return "", false
	}
	return e.budgetKey(peer), true
}
