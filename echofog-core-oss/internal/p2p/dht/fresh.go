package dht

import (
	"github.com/echofog/echofog-core/pkg/api/types"
)

// KnownPeers lists the peers currently held in the routing table
func (d *EchoFogDHT) KnownPeers() []types.PeerID {
	return d.routingTable.AllPeerIDs()
}

// ForgetLookups drops what recent lookups came to, so the next ask of any key walks the network
// again: for a scan the user asked for by hand.
func (d *EchoFogDHT) ForgetLookups() {
	d.recent.Clear()
	d.misses.Clear()
}

// AddKnownPeer puts a peer into the routing table, as learning of it from another node would.
func (d *EchoFogDHT) AddKnownPeer(pub types.PeerPublicKey, addr string) {
	d.routingTable.AddOrUpdatePeer(pub, addr)
}

// GetFresh asks the network and skips the records held on disk, so an expired local copy cannot
// shadow a newer remote record. What a lookup came to in the last seconds is reused.
func (d *EchoFogDHT) GetFresh(key []byte) ([]byte, error) {
	return d.findValue(key)
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
