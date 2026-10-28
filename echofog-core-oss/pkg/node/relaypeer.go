package node

import (
	"crypto/rand"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// relayPeersWanted is how many relays a node dials at once when it has none.
const relayPeersWanted = 3

// keepRelayPeer makes sure the node is connected to at least one relay. A node whose peers are
// all signaling nodes (which carry no traffic) could reach peers behind symmetric NATs neither
// directly nor through a relay: it dials a few nodes the DHT knows, and keeps those that relay.
func (n *Node) keepRelayPeer() {
	if n.Swarm == nil || n.routingTable == nil {
		return
	}
	peers := n.Swarm.GetAllPeers()
	if len(peers) == 0 {
		return // rejoin handles a node without any peer
	}
	connected := make(map[types.PeerID]bool, len(peers))
	for _, p := range peers {
		if p.Relays() {
			return
		}
		connected[p.ID()] = true
	}
	var target types.PeerID
	_, _ = rand.Read(target[:]) // a random spot of the ID space: different nodes each round
	dialed := 0
	for _, c := range n.routingTable.GetClosestPeers(target, 32) {
		if dialed >= relayPeersWanted {
			break
		}
		if c.ID == n.ID || connected[c.ID] || c.Addr == "" || n.Swarm.CheckOnBan(c.ID) {
			continue
		}
		if n.Swarm.GuardDialRefused(c.ID, c.Addr) {
			continue // guard mode: not across the border outside the guards
		}
		n.Swarm.AddPotentialPeer(c.ID, c.Addr)
		dialed++
	}
	if dialed > 0 {
		n.Logger.Debug("Swarm: no relay among the peers, dialing nodes from the DHT", "dialed", dialed)
	}
}
