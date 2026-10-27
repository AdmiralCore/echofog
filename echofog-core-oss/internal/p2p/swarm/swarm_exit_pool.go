package swarm

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// exitPoolState marks a peer the local node tunnels through as an exit that runs a parallel
// session pool. maxConns caps how many concurrent connections the swarm keeps to it. accumulate
// latches true once the tunnel is established (the first rotation dial): before that the first
// connection is set up normally (including a noise->REALITY upgrade of the mesh link to a
// camouflage exit, which must replace, not accumulate); after it, each further dial adds a
// parallel link instead of replacing the single one.
type exitPoolState struct {
	maxConns   int
	accumulate atomic.Bool
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

// UnmarkExitPeer forgets that exit ran a parallel pool, so future connections to it follow the
// mesh one-connection rule again. Existing extra links are left to drain on their own.
func (s *Swarm) UnmarkExitPeer(exit types.PeerID) {
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	delete(s.exitPools, exit)
}

// SetExitAccumulate latches a marked exit into accumulate mode: from now on a fresh dial to it
// adds a parallel connection instead of replacing the existing one. The exit gateway calls it on
// a rotation dial (once the tunnel is already established), never on first contact.
func (s *Swarm) SetExitAccumulate(exit types.PeerID) {
	s.exitMu.RLock()
	st := s.exitPools[exit]
	s.exitMu.RUnlock()
	if st != nil {
		st.accumulate.Store(true)
	}
}

func (s *Swarm) exitPoolFor(exit types.PeerID) *exitPoolState {
	s.exitMu.RLock()
	defer s.exitMu.RUnlock()
	return s.exitPools[exit]
}

// tryAttachExitLink attaches an outbound connection to a marked exit as an extra parallel link on
// its existing peer, instead of letting addPeer replace the single connection. It returns true
// only when it took ownership of event.Conn (attached it, or deliberately rejected it because the
// pool is full and every link is busy). It returns false — leaving the connection for addPeer — on
// first contact (no primary yet, so the mesh/REALITY connection is set up normally) and for any
// peer that is not a marked, accumulating exit.
func (s *Swarm) tryAttachExitLink(event network.NewConnEvent) bool {
	if !event.IsOut {
		return false // only our own rotation dials accumulate; inbound follows the mesh rule
	}
	st := s.exitPoolFor(event.PeerID)
	if st == nil || !st.accumulate.Load() {
		return false
	}

	s.mu.RLock()
	p, exists := s.activePeers[event.PeerID]
	s.mu.RUnlock()
	if !exists || !p.Alive() {
		return false // no live primary yet: let addPeer establish it
	}

	// A camouflage exit must carry its tunnel only over REALITY connections: a noise link would
	// show a censor the bulk bytes "in the clear". Never accumulate a noise connection to one, and
	// if the primary is still noise, hand this REALITY connection to addPeer so it replaces (and
	// upgrades) the primary instead of sitting beside a noise one.
	if s.IsCamouflagePeer(event.PeerID) {
		if !event.Reality {
			_ = event.Conn.CloseWithError(network.ErrCodeNormalClose, "non-REALITY link to camouflage exit")
			return true
		}
		if !p.primaryIsReality() {
			return false
		}
	}

	if p.LinkCount() >= st.maxConns {
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
		"link", network.DescribeLink(event.Conn), "links", p.LinkCount(), "max", st.maxConns)
	return true
}

// OpenExitStream opens one bidirectional stream to a marked exit, spreading it across the parallel
// connections of its session pool (the least-loaded live link). For a camouflage exit only
// QUIC-REALITY links are used, so a tunnel stream never rides a noise connection a censor would
// flag. For a peer with no extra links it is the primary connection, so it behaves exactly like
// opening a single stream.
func (s *Swarm) OpenExitStream(ctx context.Context, exit types.PeerID) (*network.Stream, error) {
	p := s.GetPeer(exit)
	if p == nil {
		return nil, errors.New("exit peer is not connected")
	}
	return p.openTunnelStream(ctx, s.IsCamouflagePeer(exit))
}
