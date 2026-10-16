package dht

import (
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// routingTableKey is where the routing table is kept between runs, so a restarted node knows
// the network beyond its bootstrap list at once.
var routingTableKey = []byte("dht:rt:v1")

// Save writes the table to storage.
func (r *RoutingTable) Save() error {
	if r.storage == nil {
		return nil
	}
	list := &internal_pb.PeerList{}
	for _, p := range r.allPeers() {
		id, pub := p.ID, p.PubKey
		list.Peers = append(list.Peers, &internal_pb.PeerList_Peer{Id: id[:], PubKey: pub[:], LastKnownAddr: p.Addr})
	}
	raw, err := proto.Marshal(list)
	if err != nil {
		return err
	}
	return r.storage.Set(routingTableKey, raw)
}

// Load puts the peers saved by Save back into the table and returns how many it took. The
// address policy applies as to any other entry.
func (r *RoutingTable) Load() int {
	if r.storage == nil {
		return 0
	}
	raw, err := r.storage.Get(routingTableKey)
	if err != nil || len(raw) == 0 {
		return 0
	}
	var list internal_pb.PeerList
	if proto.Unmarshal(raw, &list) != nil {
		return 0
	}
	before := r.Len()
	// oldest first, so the most recently seen end up at the fresh end of their buckets
	for i := len(list.Peers) - 1; i >= 0; i-- {
		pub, err := types.ToPeerPubKey(list.Peers[i].GetPubKey())
		if err != nil {
			continue
		}
		r.AddOrUpdatePeer(pub, list.Peers[i].GetLastKnownAddr())
	}
	return r.Len() - before
}
