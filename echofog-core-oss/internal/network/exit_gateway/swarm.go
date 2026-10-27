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

// ResetSwarmPools closes and clears all cached session pools (useful for tests and resets).
func ResetSwarmPools() {
	swarmPoolsMu.Lock()
	defer swarmPoolsMu.Unlock()
	for k, p := range swarmPools {
		_ = p.Close()
		if k.sw != nil {
			k.sw.UnmarkExitPeer(k.exit)
		}
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

	// Let the swarm keep several concurrent connections to this exit (a parallel session pool)
	// instead of one. Tunnel streams then spread across them, so a single stalled or
	// censor-killed connection does not stall the VPN. Mesh peers are never marked.
	sw.MarkExitPeer(exit, cfg.MaxConnections)

	swarmPoolsMu.Lock()
	pool, ok := swarmPools[key]
	if !ok || pool.IsClosed() {
		var pPool *PeerSessionPool
		sessionDialer := func(ctx context.Context) (StreamOpener, func() error, error) {
			camouflage := sw.IsCamouflagePeer(exit)
			hasSessions := pPool != nil && pPool.TotalSessions() > 0
			if hasSessions {
				// The tunnel is already up on an established connection, so this dial is a
				// rotation: have the swarm add a fresh parallel link rather than replace the
				// existing one. First contact (hasSessions == false) is left alone so the initial
				// mesh/REALITY connection is set up normally.
				sw.SetExitAccumulate(exit)
			}
			// A rotation dials a fresh connection to the exit to get a new 5-tuple. The swarm keeps
			// one connection per peer, so that dial replaces and (after a drain timeout) force-closes
			// the connection the current traffic rides. A single large download is one stream that
			// cannot be split across connections, so rotating while it is in flight cuts it off. Defer
			// the rotation dial until no stream is active; a long transfer then holds the connection
			// until it finishes and rotation resumes once it drains. First contact with a camouflage
			// exit is not a rotation — it must still dial its REALITY connection to replace the reused
			// noise one — so only a rotation is gated, never that first dial.
			noActive := pPool == nil || !pPool.HasActiveStreams()
			dialCamouflage := camouflage && (!hasSessions || noActive)
			dialRotate := !camouflage && hasSessions && noActive
			if dialCamouflage || dialRotate {
				p := sw.GetPeer(exit)
				if p != nil {
					addr := p.Addr()
					if addr == "" {
						addr = p.RemoteAddr()
					}
					if addr != "" {
						switch {
						case dialCamouflage:
							// The exit is a camouflage node (it advertises a cover SNI). Its tunnel must
							// run over a REALITY connection, not a noise connection reused from the mesh
							// bootstrap: a noise connection to a camouflage node does not serve the tunnel
							// and the exit goes silent (the client idles out and moves to another exit).
							// Register the SNI so the dial uses REALITY and dial — on first use to replace
							// a reused noise connection, on rotation to rotate the REALITY connection. A
							// REALITY dial always gets its own fresh socket/5-tuple, so this also gives the
							// rotation a distinct flow without the ephemeral-socket marker.
							sw.ExpectPeerAddr(addr, exit)
							_ = sw.DialEarly(ctx, addr)
						case dialRotate:
							// Non-camouflage exit, rotating: dial a fresh connection to rotate the physical
							// socket/5-tuple. The ephemeral-socket marker gives even the noise path a new
							// 5-tuple, so a rotated noise tunnel is a distinct flow to a censor rather than
							// reusing the main socket.
							_ = sw.DialEarly(network.WithEphemeralSocket(ctx), addr)
						}
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
