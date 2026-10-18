package exit_gateway

import (
	"context"
	"io"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// swarmStream is a peer stream; sw lets a UDP session on it reach the peer with datagrams.
type swarmStream struct {
	*network.Stream
	sw *swarm.Swarm
}

func (s swarmStream) Read(p []byte) (int, error)  { return s.Stream.Read(p) }
func (s swarmStream) Write(p []byte) (int, error) { return s.Stream.Write(p) }

var _ io.ReadWriteCloser = swarmStream{}

// AttachToSwarm makes this node answer incoming exit tunnel requests (they are refused unless the service is enabled)
func AttachToSwarm(sw *swarm.Swarm, svc *ExitService) {
	sw.RegisterStreamHandler(net_types.TypeExitConnect, func(st *network.Stream, target []byte, peer types.PeerID) {
		svc.HandleTunnelFrom(context.Background(), swarmStream{st, sw}, target, peer)
	})
	sw.RegisterStreamHandler(net_types.TypeExitAccount, func(st *network.Stream, _ []byte, peer types.PeerID) {
		svc.HandleAccountFrom(context.Background(), swarmStream{st, sw}, peer)
	})
	sw.RegisterStreamHandler(net_types.TypeExitVoucher, func(st *network.Stream, offer []byte, peer types.PeerID) {
		svc.HandleVoucherFrom(context.Background(), swarmStream{st, sw}, offer, peer)
	})
	sw.RegisterStreamHandler(net_types.TypeExitUDP, func(st *network.Stream, hello []byte, peer types.PeerID) {
		svc.handleUDP(context.Background(), swarmStream{st, sw}, peer, hello)
	})
}

// SwarmOpener opens tunnel streams to one specific exit peer
func SwarmOpener(sw *swarm.Swarm, exit types.PeerID) StreamOpener {
	return func(ctx context.Context) (FramedStream, error) {
		streams, err := sw.OpenNewStreams(ctx, exit, 1)
		if err != nil {
			return nil, err
		}
		return swarmStream{streams[0], sw}, nil
	}
}
