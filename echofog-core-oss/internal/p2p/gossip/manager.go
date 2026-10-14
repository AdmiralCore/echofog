package gossip

import (
	"sync"

	types2 "github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type Manager struct {
	swarm *swarm.Swarm

	mu        sync.RWMutex
	seenCache map[types.MessageID]bool
}

func NewManager(swarm *swarm.Swarm) *Manager {
	return &Manager{
		swarm:     swarm,
		seenCache: make(map[types.MessageID]bool),
	}
}

func (g *Manager) Broadcast(msgType types2.MessageType, msgData *internal_pb.MessageData) {
	mesID, err := types.ParseMessageID(msgData.GetMessageId())
	if err != nil {
		return
	}
	g.mu.Lock()
	if g.seenCache[mesID] {
		g.mu.Unlock()
		return
	}
	g.seenCache[mesID] = true
	g.mu.Unlock()

	msgData.HopLimit--
	if msgData.HopLimit <= 0 {
		return
	}

	peers := g.swarm.GetAllPeers()

	for _, peer := range peers {

		originID, _ := types.ToPeerID(msgData.OriginId)
		if peer.ID() == originID {
			continue
		}

		go func(pID types.PeerID) {
			if g.swarm.GetContext().Err() != nil {
				return
			}
			g.swarm.SendDataForPeer(pID, msgType, msgData)
		}(peer.ID())
	}
}

func (g *Manager) HandleIncoming(msgType types2.MessageType, msgData *internal_pb.MessageData) {
	mesID, err := types.ParseMessageID(msgData.GetMessageId())
	if err != nil {
		return
	}
	g.mu.RLock()
	seen := g.seenCache[mesID]
	g.mu.RUnlock()

	if seen {
		return
	}

	g.Broadcast(msgType, msgData)
}
