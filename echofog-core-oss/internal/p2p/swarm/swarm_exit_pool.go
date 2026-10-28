package swarm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// exitPoolState marks a peer the local node tunnels through as an exit that runs a parallel
// session pool. maxConns caps how many concurrent connections the swarm keeps to it.
type exitPoolState struct {
	freshMu  sync.Mutex // one fresh-link dial at a time (OpenExitStream)
	maxConns int
	addr     string        // where the exit was last reached, to redial it once its peer is gone; exitMu
	stop     chan struct{} // ends the retire loop when the exit is unmarked
}

// retireEvery is how often a marked exit's old, idle links are looked for and closed.
const retireEvery = time.Second

// A new tunnel stream never starts on a link older than freshLinkAge: a fresh pool link is dialed
// first (waiting up to freshLinkWait for it). Otherwise a link left alone when the newer ones
// ended would take the next burst of traffic and become, to a censor, one connection that lived
// for minutes and carried tens of megabytes — what flow shaping exists to prevent.
const (
	freshLinkAge  = time.Minute
	freshLinkWait = 3 * time.Second
)

// maxInboundPoolLinks caps how many connections the exit side keeps to one client that opens
// parallel pool links: the client's own cap plus one, for the fresh link the client attaches
// before it retires an old one. The exit never closes a client's link to make room — the client
// may be opening a stream on it — it refuses the new one instead.
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
	st := &exitPoolState{maxConns: maxConns, stop: make(chan struct{})}
	s.exitPools[exit] = st
	s.wg.Add(1)
	go s.retireLoop(exit, st.stop)
}

// retireLoop closes the old links to a marked exit once they are idle (see Peer.retireOldLinks):
// a link whose transfers ended while no new link came would otherwise live on.
func (s *Swarm) retireLoop(exit types.PeerID, stop <-chan struct{}) {
	defer s.wg.Done()
	t := time.NewTicker(retireEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if p := s.GetPeer(exit); p != nil {
			p.retireOldLinks(s.IsCamouflagePeer(exit))
		}
	}
}

// UnmarkExitPeer forgets that exit ran a parallel pool and closes the extra links to it (the other
// end prunes them as they close), so the exit is again a mesh peer with one connection.
func (s *Swarm) UnmarkExitPeer(exit types.PeerID) {
	s.exitMu.Lock()
	if st := s.exitPools[exit]; st != nil {
		close(st.stop)
		delete(s.exitPools, exit)
	}
	s.exitMu.Unlock()
	if p := s.GetPeer(exit); p != nil {
		p.closeExitLinks()
	}
}

// exitAddr is the address p is reached at, remembered for a marked exit so it can be redialed after
// the peer is gone; with p nil it is the remembered one.
func (s *Swarm) exitAddr(exit types.PeerID, p *Peer) string {
	addr := ""
	if p != nil {
		if addr = p.Addr(); addr == "" {
			addr = p.RemoteAddr()
		}
	}
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	st := s.exitPools[exit]
	switch {
	case st == nil:
	case addr != "":
		st.addr = addr
	default:
		addr = st.addr
	}
	return addr
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

	camouflage := event.IsOut && s.IsCamouflagePeer(event.PeerID)
	if event.IsOut && p.LinkCount() >= maxConns {
		p.retireOldLinks(camouflage) // make room by closing old idle links, never a busy one
	}
	if p.LinkCount() >= maxConns {
		// Still full: every link carries a transfer (or, on the exit side, the client keeps its
		// links). Refuse the new link rather than close one a stream may be on; the pool dials again
		// at its next rotation.
		_ = event.Conn.CloseWithError(network.ErrCodeNormalClose, "exit pool full")
		return true
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

	if event.IsOut {
		// The fresh link now takes the new streams; the older ones are closed as soon as they are idle.
		p.retireOldLinks(camouflage)
	}
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
	if st := s.exitPoolFor(exit); st != nil {
		s.freshenExitLink(ctx, exit, st, camouflage)
		if np := s.GetPeer(exit); np != nil {
			p = np
		}
	}
	return p.openTunnelStream(ctx, camouflage)
}

// freshenExitLink dials a fresh pool link to a marked exit when the link a new stream would ride
// is older than freshLinkAge, and waits a little for it. If none comes up in time the stream
// rides the old link: an old link beats a failed stream.
func (s *Swarm) freshenExitLink(ctx context.Context, exit types.PeerID, st *exitPoolState, camouflage bool) {
	stale := func() bool {
		p := s.GetPeer(exit)
		if p == nil {
			return false
		}
		age, ok := p.newestLinkAge(camouflage)
		return ok && age > freshLinkAge
	}
	if !stale() {
		return
	}
	st.freshMu.Lock()
	defer st.freshMu.Unlock()
	if !stale() { // another stream freshened it meanwhile
		return
	}
	p := s.GetPeer(exit)
	if p == nil {
		return
	}
	if p.retireOldLinks(camouflage); p.LinkCount() >= st.maxConns {
		return // every link carries a transfer: a new one would be refused, do not wait for it
	}
	addr := s.exitAddr(exit, p)
	if addr == "" {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, freshLinkWait)
	defer cancel()
	s.dialPoolLink(dctx, exit, addr, camouflage)
	tick := time.NewTicker(camouflageLinkPoll)
	defer tick.Stop()
	for stale() {
		select {
		case <-dctx.Done():
			return
		case <-tick.C:
		}
	}
}

// dialPoolLink dials an extra parallel link to the exit at addr, announced as a pool link so both
// ends keep it beside the others: REALITY (its own socket) for a camouflage exit, otherwise noise
// on a socket of its own.
func (s *Swarm) dialPoolLink(ctx context.Context, exit types.PeerID, addr string, camouflage bool) {
	dctx := network.WithPoolLink(ctx)
	if camouflage {
		_ = s.expectPeer(addr, exit)
	} else {
		dctx = network.WithEphemeralSocket(dctx)
	}
	if err := s.dialEarly(dctx, addr); err != nil {
		s.log().Debug("Swarm: pool link dial to exit failed", "peer", exit.String()[:12], "err", err)
	}
}

const (
	camouflageLinkPoll   = 20 * time.Millisecond // how often a dialed link is looked for
	camouflageLinkSettle = time.Second           // how long it may take to register before redialing
)

// dialCamouflaged dials the camouflage exit at addr with its SNI registered, so the connection is
// REALITY, or browser-TLS mimicry over TCP when QUIC does not get through.
func (s *Swarm) dialCamouflaged(ctx context.Context, exit types.PeerID, addr string) {
	_ = s.expectPeer(addr, exit)
	if err := s.dialEarly(ctx, addr); err != nil {
		s.log().Debug("Swarm: camouflaged dial to exit failed", "peer", exit.String()[:12], "err", err)
	}
}

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
		// with the peer gone (its link died, e.g. UDP was cut), this is the address it was last at
		addr := s.exitAddr(exit, p)
		if p != nil && p.hasCamouflagedLink() {
			return nil
		}
		dialed := false
		if addr != "" {
			s.log().Info("Swarm: camouflage exit has no camouflaged link, dialing one", "peer", exit.String()[:12], "addr", addr)
			s.dialCamouflaged(ctx, exit, addr)
			dialed = true
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
