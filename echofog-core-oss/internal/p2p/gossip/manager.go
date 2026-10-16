package gossip

import (
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/cache"
	types2 "github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type Manager struct {
	swarm *swarm.Swarm

	mu        sync.Mutex // makes check-and-mark of a message one step
	seenCache *cache.Cache[types.MessageID, struct{}]
}

// A message is remembered long enough to outlive its trip through the network, and the
// number remembered is bounded: a flood of new IDs must not grow the memory without end.
const (
	seenCacheSize = 1 << 16
	seenCacheTTL  = 30 * time.Minute
)

func NewManager(swarm *swarm.Swarm) *Manager {
	return &Manager{
		swarm:     swarm,
		seenCache: cache.New[types.MessageID, struct{}](seenCacheSize, seenCacheTTL),
	}
}

func (g *Manager) Broadcast(msgType types2.MessageType, msgData *internal_pb.MessageData) {
	mesID, err := types.ParseMessageID(msgData.GetMessageId())
	if err != nil {
		return
	}
	g.mu.Lock()
	if g.seenCache.Exists(mesID) {
		g.mu.Unlock()
		return
	}
	g.seenCache.Set(mesID, struct{}{})
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
	if g.seenCache.Exists(mesID) {
		return
	}

	g.Broadcast(msgType, msgData)
}
