package node

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// MeasureUploadCapacity probes connected peers for real throughput, applies it as the node's upload capacity and returns kbps
func (n *NodeApi) MeasureUploadCapacity() (int, error) {
	if n.node == nil || n.node.Swarm == nil || n.node.BWProbe == nil {
		return 0, errors.New("node is not running")
	}

	var peers []types.PeerID
	for _, p := range n.node.Swarm.GetAllPeers() {
		peers = append(peers, p.ID())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	kbps, err := n.node.BWProbe.MeasureBest(ctx, peers, 3)
	if err != nil {
		return 0, err
	}
	if err := n.SetUploadCapacity(kbps); err != nil {
		return 0, err
	}
	return kbps, nil
}
