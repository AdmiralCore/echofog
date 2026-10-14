package bwprobe

import (
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type swarmTransport struct{ s *swarm.Swarm }

func (t swarmTransport) SendRawFrame(peer types.PeerID, mt net_types.MessageType, data []byte) error {
	return t.s.SendRawFrame(peer, mt, data)
}

func (t swarmTransport) RegisterFastFrameHandler(mt net_types.MessageType, h func([]byte, types.PeerID) error) {
	t.s.RegisterFastFrameHandler(mt, h)
}

// NewForSwarm attaches a prober to a live swarm
func NewForSwarm(s *swarm.Swarm) *Prober { return New(swarmTransport{s}) }
