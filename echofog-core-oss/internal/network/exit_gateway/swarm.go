package exit_gateway

import (
	"context"
	"io"
	"sync"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// swarmPadsPeer reports whether tunnels between this node and peer id use the TLS-in-TLS padding:
// both this node's toggle is on (so it announced CapTunnelPadding and will strip padding) and the
// peer announced the same capability. Both ends compute this identically, so a peer pair agrees.
func swarmPadsPeer(sw *swarm.Swarm, id types.PeerID) bool {
	if sw == nil || !sw.PadsTunnels() {
		return false
	}
	p := sw.GetPeer(id)
	return p != nil && p.Caps().Has(handshake.CapTunnelPadding)
}

// NewSwarmP2PDialer builds a P2PDialer to exit over sw, with the TLS-in-TLS padding gated on the
// exit peer's advertised capability. Prefer this over NewP2PDialer(SwarmOpener(...)) so every
// tunnel to a padding-capable exit is padded consistently.
func NewSwarmP2PDialer(sw *swarm.Swarm, exit types.PeerID) *P2PDialer {
	d := NewP2PDialer(SwarmOpener(sw, exit))
	d.pad = func() bool { return swarmPadsPeer(sw, exit) }
	return d
}

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
	svc.SetPadData(func(id types.PeerID) bool { return swarmPadsPeer(sw, id) })
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

// ReleaseSwarmPool drops the session pool to exit, once the proxy no longer tunnels through it (the
// VPN stopped or moved to another exit): its sessions are closed, the parallel links to the exit are
// closed, and the exit is again an ordinary peer with one connection. Without this, every exit ever
// used kept its pool links open. Safe to call for an exit with no pool.
func ReleaseSwarmPool(sw *swarm.Swarm, exit types.PeerID) {
	if sw == nil {
		return
	}
	key := swarmPoolKey{sw: sw, exit: exit}
	swarmPoolsMu.Lock()
	pool := swarmPools[key]
	delete(swarmPools, key)
	swarmPoolsMu.Unlock()
	if pool != nil {
		_ = pool.Close()
	}
	sw.UnmarkExitPeer(exit)
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

	// Let the swarm keep several concurrent connections to this exit (a parallel session pool)
	// instead of one. Tunnel streams then spread across them, so a single stalled or
	// censor-killed connection does not stall the VPN. Mesh peers are never marked.
	sw.MarkExitPeer(exit, cfg.MaxConnections)

	swarmPoolsMu.Lock()
	pool, ok := swarmPools[key]
	if !ok || pool.IsClosed() {
		var pPool *PeerSessionPool
		sessionDialer := func(ctx context.Context) (StreamOpener, func() error, error) {
			// The first session rides the connection the swarm already has to the exit; for a
			// camouflage exit OpenExitStream first makes sure that connection is camouflaged
			// (REALITY or TCP mimicry). Every later session is a rotation: it dials a fresh parallel
			// link to the same exit, announced as a pool link so both ends keep it beside the
			// existing ones instead of replacing them. The new session gets its own 5-tuple, the
			// load spreads, and no stream in flight is cut off (the pool only ever drains an idle
			// link to make room), so rotation need not wait for transfers to finish.
			if pPool != nil && pPool.TotalSessions() > 0 {
				if p := sw.GetPeer(exit); p != nil {
					addr := p.Addr()
					if addr == "" {
						addr = p.RemoteAddr()
					}
					if addr != "" {
						dctx := network.WithPoolLink(ctx)
						if sw.IsCamouflagePeer(exit) {
							// register the SNI so the link is REALITY, which always has its own socket
							sw.ExpectPeerAddr(addr, exit)
						} else {
							// a noise link needs its own socket asked for, or it shares the main one
							dctx = network.WithEphemeralSocket(dctx)
						}
						_ = sw.DialEarly(dctx, addr)
					}
				}
			}
			op := func(ctx context.Context) (FramedStream, error) {
				// Spread the stream across the exit's parallel connections (least-loaded link),
				// so one connection does not carry all tunnel traffic. With a single connection
				// this is identical to opening one stream on it.
				st, err := sw.OpenExitStream(ctx, exit)
				if err != nil {
					return nil, err
				}
				return swarmStream{st, sw}, nil
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
