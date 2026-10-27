package swarm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// exitPoolState marks a peer the local node tunnels through as an exit that runs a parallel
// session pool. maxConns caps how many concurrent connections the swarm keeps to it.
type exitPoolState struct {
	maxConns int
}

// maxInboundPoolLinks caps how many connections the exit side keeps to one client that opens
// parallel pool links: the client's own cap plus one, for a fresh link that arrives before the
// client has closed the idle one it replaces.
const maxInboundPoolLinks = 4

// quicNoise reports whether a connection is a QUIC noise-obfs link — the only kind unsafe to carry
// tunnel traffic to a camouflage exit. A QUIC-REALITY link (reality) and any TCP/WS link (the TCP
// fallback is itself browser-TLS mimicry, so it is already camouflaged) are both fine; only a plain
// QUIC noise link would show a censor the bulk bytes as "random". proto is MuxConn.Protocol().
func quicNoise(proto string, reality bool) bool {
	return proto == network.ProtocolQUIC && !reality
}

// MarkExitPeer marks exit as a peer that runs a parallel session pool of up to maxConns concurrent
// connections. The exit gateway calls it when it builds the pool. Mesh peers are never marked and
// keep strict one-connection-per-peer semantics. Marking is idempotent; it only updates the cap.
func (s *Swarm) MarkExitPeer(exit types.PeerID, maxConns int) {
	if maxConns < 1 {
		maxConns = 1
	}
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	if s.exitPools == nil {
		s.exitPools = make(map[types.PeerID]*exitPoolState)
	}
	if st, ok := s.exitPools[exit]; ok {
		st.maxConns = maxConns
		return
	}
	s.exitPools[exit] = &exitPoolState{maxConns: maxConns}
}

// UnmarkExitPeer forgets that exit ran a parallel pool and closes the extra links to it (the other
// end prunes them as they close), so the exit is again a mesh peer with one connection.
func (s *Swarm) UnmarkExitPeer(exit types.PeerID) {
	s.exitMu.Lock()
	delete(s.exitPools, exit)
	s.exitMu.Unlock()
	if p := s.GetPeer(exit); p != nil {
		p.closeExitLinks()
	}
}

func (s *Swarm) exitPoolFor(exit types.PeerID) *exitPoolState {
	s.exitMu.RLock()
	defer s.exitMu.RUnlock()
	return s.exitPools[exit]
}

// tryAttachExitLink attaches a pool link (event.PoolLink) as an extra parallel connection on the
// existing peer, instead of letting addPeer replace the single connection. Both ends do this for
// the same connection: the client because its exit pool dialed it, the exit because the client
// announced it in the handshake — so neither end closes the connection the other keeps using. It
// returns true only when it took ownership of event.Conn (attached it, or deliberately rejected it
// because the pool is full and every link is busy). It returns false — leaving the connection for
// addPeer — for an ordinary connection and when there is no live connection to the peer yet (the
// pool link then simply becomes the primary).
func (s *Swarm) tryAttachExitLink(event network.NewConnEvent) bool {
	if !event.PoolLink {
		return false
	}
	maxConns := maxInboundPoolLinks
	if event.IsOut {
		st := s.exitPoolFor(event.PeerID)
		if st == nil {
			return false // the pool was torn down meanwhile
		}
		maxConns = st.maxConns
	}

	s.mu.RLock()
	p, exists := s.activePeers[event.PeerID]
	s.mu.RUnlock()
	if !exists || !p.Alive() {
		return false // no live primary yet: let addPeer establish it
	}

	// A camouflage exit must carry its tunnel only over camouflaged connections — QUIC-REALITY or a
	// TCP/WS mimicry link — never a plain QUIC noise link, which would show a censor the bulk bytes
	// "in the clear". Reject a QUIC noise connection, and if the primary is not camouflaged (a stale
	// QUIC noise link), hand this one to addPeer so it replaces (upgrades) the primary instead of
	// sitting beside it.
	if s.IsCamouflagePeer(event.PeerID) {
		if quicNoise(event.Conn.Protocol(), event.Reality) {
			_ = event.Conn.CloseWithError(network.ErrCodeNormalClose, "QUIC noise link to camouflage exit")
			return true
		}
		if !p.primaryIsCamouflaged() {
			return false
		}
	}

	if p.LinkCount() >= maxConns {
		// At capacity: rotate by draining an old idle link to free a slot. Never drain a link
		// with a stream in flight — a transfer must not be cut off mid-way (the same guarantee
		// the pool's rotation gate relies on). If every extra link is busy, reject this dial and
		// keep the current connections; the pool retries once a link frees up.
		victim := p.oldestIdleExtraLink()
		if victim == nil {
			_ = event.Conn.CloseWithError(network.ErrCodeNormalClose, "exit pool full")
			return true
		}
		p.removeExitLink(victim)
		_ = victim.Close()
	}

	pw := network.NewPeerWrapper(p.ctx, event.Conn, event.PeerID)
	s.attachStreamHandlers(pw, p, event.PeerID)
	p.addExitLink(pw, event.Reality)
	go pw.StartLoops()

	// Prune this link from the peer when its connection dies, so a stalled or censor-killed
	// link is dropped while the other pool members keep carrying traffic. The peer itself and
	// its primary connection are untouched.
	conn := event.Conn
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-conn.Context().Done():
		case <-p.ctx.Done():
			return // peer closing already closes every extra link
		case <-s.ctx.Done():
			return
		}
		p.removeExitLink(pw)
		_ = pw.Close()
		s.log().Debug("Swarm: exit pool link closed", "peer", event.PeerID.String()[:12],
			"link", network.DescribeLink(conn), "links_left", p.LinkCount())
	}()

	s.log().Debug("Swarm: added parallel exit link", "peer", event.PeerID.String()[:12],
		"link", network.DescribeLink(event.Conn), "links", p.LinkCount(), "max", maxConns)
	return true
}

// OpenExitStream opens one bidirectional stream to a marked exit, spreading it across the parallel
// connections of its session pool (the least-loaded live link). For a camouflage exit only
// camouflaged links (QUIC-REALITY or TCP mimicry) are used, so a tunnel stream never rides a noise
// connection a censor would flag; when it has none, one is dialed and waited for (see
// ensureCamouflagedLink) instead of failing the stream. For a peer with no extra links it is the
// primary connection, so it behaves exactly like opening a single stream.
func (s *Swarm) OpenExitStream(ctx context.Context, exit types.PeerID) (*network.Stream, error) {
	camouflage := s.IsCamouflagePeer(exit)
	if camouflage {
		if err := s.ensureCamouflagedLink(ctx, exit); err != nil {
			return nil, err
		}
	}
	p := s.GetPeer(exit)
	if p == nil {
		return nil, errors.New("exit peer is not connected")
	}
	return p.openTunnelStream(ctx, camouflage)
}

const (
	camouflageLinkPoll   = 20 * time.Millisecond // how often a dialed link is looked for
	camouflageLinkSettle = time.Second           // how long it may take to register before redialing
)

// ensureCamouflagedLink makes sure a camouflage exit has a live camouflaged link (QUIC-REALITY or
// TCP mimicry) and, if it has none, dials one and waits until the swarm has registered it, dialing
// again until ctx ends. A one-shot upgrade at VPN start is not enough: the dial returns before its
// connection is registered (that happens later, in registrationLoop); a dial to the same address
// already in flight — the mesh redialing the exit over noise — is joined and hands back that noise
// connection; and the camouflaged link can die later. Each of these left the exit with only a noise
// link, so every tunnel stream failed and the VPN failover moved the client to another exit, whose
// tunnel then ran over noise.
func (s *Swarm) ensureCamouflagedLink(ctx context.Context, exit types.PeerID) error {
	tick := time.NewTicker(camouflageLinkPoll)
	defer tick.Stop()
	for {
		p := s.GetPeer(exit)
		if p != nil && p.hasCamouflagedLink() {
			return nil
		}
		dialed := false
		if p != nil {
			addr := p.Addr()
			if addr == "" {
				addr = p.RemoteAddr()
			}
			if addr != "" {
				s.log().Info("Swarm: camouflage exit has no camouflaged link, dialing one", "peer", exit.String()[:12], "addr", addr)
				s.expectPeer(addr, exit) // register the SNI so the dial uses REALITY (or TCP mimicry)
				if err := s.dialEarly(ctx, addr); err != nil {
					s.log().Debug("Swarm: camouflaged dial to exit failed", "peer", exit.String()[:12], "err", err)
				}
				dialed = true
			}
		}

		settle := time.NewTimer(camouflageLinkSettle)
	wait:
		for {
			select {
			case <-ctx.Done():
				settle.Stop()
				return fmt.Errorf("camouflage exit has no camouflaged (REALITY or TCP) link: %w", context.Cause(ctx))
			case <-settle.C:
				break wait
			case <-tick.C:
				if p := s.GetPeer(exit); p != nil && p.hasCamouflagedLink() {
					settle.Stop()
					return nil
				}
			}
		}
		if !dialed && s.GetPeer(exit) == nil {
			return errors.New("exit peer is not connected")
		}
	}
}
