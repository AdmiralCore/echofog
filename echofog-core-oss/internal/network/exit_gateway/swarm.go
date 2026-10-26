package exit_gateway

import (
	"context"
	"io"
	"sync"

	"github.com/echofog/echofog-core/internal/config"
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
	svc.SetAddrOf(func(id types.PeerID) string {
		if p := sw.GetPeer(id); p != nil {
			return p.RemoteAddr()
		}
		return ""
	})
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

type swarmPoolKey struct {
	sw   *swarm.Swarm
	exit types.PeerID
}

var (
	swarmPoolsMu sync.Mutex
	swarmPools   = make(map[swarmPoolKey]*PeerSessionPool)
)

// ResetSwarmPools closes and clears all cached session pools (useful for tests and resets).
func ResetSwarmPools() {
	swarmPoolsMu.Lock()
	defer swarmPoolsMu.Unlock()
	for k, p := range swarmPools {
		_ = p.Close()
		delete(swarmPools, k)
	}
}

// SwarmOpener opens tunnel streams to one specific exit peer with flow shaping and session rotation.
func SwarmOpener(sw *swarm.Swarm, exit types.PeerID) StreamOpener {
	var cfg config.FlowShapingConfig
	if sw != nil && sw.Config() != nil {
		cfg = sw.Config().AntiDPI.FlowShaping
	} else {
		cfg = config.DefaultFlowShapingConfig()
	}
	return SwarmOpenerWithConfig(sw, exit, cfg)
}

// SwarmOpenerWithConfig opens tunnel streams to one exit peer using custom flow shaping settings.
func SwarmOpenerWithConfig(sw *swarm.Swarm, exit types.PeerID, cfg config.FlowShapingConfig) StreamOpener {
	if sw == nil {
		return func(ctx context.Context) (FramedStream, error) {
			return nil, errNoStreamOpener
		}
	}

	cfg.Normalize()
	key := swarmPoolKey{sw: sw, exit: exit}

	swarmPoolsMu.Lock()
	pool, ok := swarmPools[key]
	if !ok || pool.IsClosed() {
		var pPool *PeerSessionPool
		sessionDialer := func(ctx context.Context) (StreamOpener, func() error, error) {
			if pPool != nil && pPool.TotalSessions() > 0 {
				p := sw.GetPeer(exit)
				if p != nil {
					addr := p.Addr()
					if addr == "" {
						addr = p.RemoteAddr()
					}
					if addr != "" {
						// Dial a fresh connection to exit to rotate the physical socket/5-tuple
						_ = sw.DialEarly(ctx, addr)
					}
				}
			}
			op := func(ctx context.Context) (FramedStream, error) {
				streams, err := sw.OpenNewStreams(ctx, exit, 1)
				if err != nil {
					return nil, err
				}
				return swarmStream{streams[0], sw}, nil
			}
			return op, nil, nil
		}
		pPool = NewPeerSessionPoolWithDialer(exit, cfg, sessionDialer)
		pool = pPool
		swarmPools[key] = pool
	}
	swarmPoolsMu.Unlock()

	return pool.OpenStream
}
