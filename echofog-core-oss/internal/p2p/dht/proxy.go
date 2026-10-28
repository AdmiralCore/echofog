package dht

import (
	"context"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Lookups on behalf of a user's node. A user's node in guard mode (see swarm_guards.go) does not
// dial the peers a lookup is referred to: across a censoring border each would be a flow of its
// own. It asks its guard instead, and the guard — a server, well connected to the others — walks
// the DHT itself and answers with what it found: the record, or the closest peers it reached.
// Nothing in the protocol changes: the asker sends the same FindNode and FindValue, and a guard
// that cannot spare the capacity, or an older one, answers from its own table as before.

const (
	// proxyWait bounds a lookup run for a user; the asker waits guardQueryTimeout.
	proxyWait = 4 * time.Second
	// proxyMaxActive bounds the lookups a server runs for users at once.
	proxyMaxActive = 8
)

type proxyState struct {
	mu     sync.Mutex
	active int
	busy   map[types.PeerID]bool
}

// proxyLookup runs run in the background as a lookup for peer, when this node is a server and the
// peer is a user's node, and reports whether it did. At most proxyMaxActive run at once and one
// per peer; beyond that the caller answers from its own table.
func (d *EchoFogDHT) proxyLookup(peer types.PeerID, run func(ctx context.Context)) bool {
	if !d.swarm.ServesUsers() || !d.swarm.IsUserPeer(peer) {
		return false
	}
	d.proxy.mu.Lock()
	if d.proxy.active >= proxyMaxActive || d.proxy.busy[peer] {
		d.proxy.mu.Unlock()
		return false
	}
	if d.proxy.busy == nil {
		d.proxy.busy = map[types.PeerID]bool{}
	}
	d.proxy.active++
	d.proxy.busy[peer] = true
	d.proxy.mu.Unlock()
	go func() {
		defer func() {
			d.proxy.mu.Lock()
			d.proxy.active--
			delete(d.proxy.busy, peer)
			d.proxy.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), proxyWait)
		defer cancel()
		run(ctx)
	}()
	return true
}
