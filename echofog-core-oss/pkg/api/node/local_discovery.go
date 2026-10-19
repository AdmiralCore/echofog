package node

import (
	"encoding/hex"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/dto"
)

func (n *NodeApi) StartLocalDiscovery(myAlias string) {
	if n.node.AccessMode() != config.AccessPublic {
		return // a hidden node does not announce itself, not even on the local network
	}
	err := n.node.MDNS.Start(n.node.Ctx, myAlias)
	if err != nil {
		n.node.Logger.Error("Failed to start mDNS", "err", err)
		return
	}
	n.node.Logger.Info("mDNS Discovery started by user")
}

func (n *NodeApi) StopLocalDiscovery() {
	n.node.MDNS.Stop()
}

func (n *NodeApi) PeerDiscoveryHandler() <-chan dto.PeerDTO {
	readCh := make(chan dto.PeerDTO, 100)

	n.node.MDNS.SetOnPeerFound(func(id types.PeerID, addr string, alias string) {
		idHex := hex.EncodeToString(id[:])

		dto := dto.PeerDTO{
			PeerID:      idHex,
			ShortID:     idHex[:8],
			Address:     addr,
			IsConnected: false,
			IsLocal:     true,
			LastSeen:    time.Now().Format("15:04:05"),
		}

		select {
		case readCh <- dto:
		default:
		}
	})

	return readCh
}
