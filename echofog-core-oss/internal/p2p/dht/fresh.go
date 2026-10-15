package dht

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// KnownPeers lists the peers currently held in the routing table
func (d *EchoFogDHT) KnownPeers() []types.PeerID {
	return d.routingTable.AllPeerIDs()
}

// GetFresh asks the network only and skips the local cache, so expired local copies cannot shadow newer remote records
func (d *EchoFogDHT) GetFresh(key []byte) ([]byte, error) {
	targetID, _ := types.ToPeerID(key)
	nodes := d.routingTable.GetClosestPeers(targetID, Alpha)
	if len(nodes) == 0 {
		return nil, errors.New("DHT: no peers known to search for value")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	found := make(chan []byte, len(nodes))
	lookup := newValueLookup(d.swarm.GetSelfID())
	for _, node := range nodes {
		if !lookup.claim(node.ID) {
			continue
		}
		go func(p *PeerNode) {
			if rec, ok := d.queryPeerForValue(ctx, lookup, p.ID, key); ok && rec != nil {
				found <- rec.Value
			}
		}(node)
	}

	select {
	case v := <-found:
		return v, nil
	case <-ctx.Done():
		return nil, errors.New("value not found in network")
	}
}

// AllPeerIDs returns every peer currently stored across all buckets
func (r *RoutingTable) AllPeerIDs() []types.PeerID {
	var out []types.PeerID
	for _, b := range r.buckets {
		if b == nil {
			continue
		}
		b.mu.RLock()
		for _, p := range b.Peers {
			out = append(out, p.ID)
		}
		b.mu.RUnlock()
	}
	return out
}
