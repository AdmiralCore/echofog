package swarm

import (
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/abuse"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Limits on what peers may hold of this node. Peer IDs are free to make, so every per-peer limit
// here is backed by one per address in the shared Guard (handshake.Guard.AdmitPeer).
const (
	DefaultMaxInboundPeers       = 256
	DefaultServerMaxInboundPeers = 2048

	// maxPendingStreamsPerPeer: streams a peer opened whose first frame has not come yet. A peer
	// that opens streams and sends nothing would otherwise hold a goroutine per stream (for at
	// most the 5 s first-frame deadline; QUIC caps a peer's open streams at MaxBidiStreamsPerPeer
	// anyway). It was 64: a VPN client carries every tab of a browser over one link to its exit,
	// and a page that opened a hundred connections had some closed without a word and its user
	// scored as a flood, which a few busy minutes turned into a ban from the exit.
	maxPendingStreamsPerPeer = 512
	// maxStreamHandlers bounds the goroutines serving streams of all peers together.
	maxStreamHandlers = 16384

	// longLivedPeer: a peer that stayed connected this long counts as known for knownFor, so a
	// full node lets it back in and a server asks it no puzzle.
	longLivedPeer = 10 * time.Minute
	knownFor      = 24 * time.Hour
	maxRecent     = 4096
)

var errInboundFull = errors.New("inbound peers full")

// limits is the swarm's part of flood protection.
type limits struct {
	guard   atomic.Pointer[handshake.Guard]
	scorer  *abuse.Scorer
	trusted atomic.Pointer[func(types.PeerID) bool]

	recentMu sync.Mutex
	recent   map[types.PeerID]time.Time // long-lived peers, until when they count as known

	streams atomic.Int64 // stream handlers running, all peers together
}

// guardOwner is a transport that limits inbound connections per address.
type guardOwner interface {
	Guard() *handshake.Guard
}

type guardSetter interface {
	SetGuard(*handshake.Guard)
}

type screenSetter interface {
	SetScreen(handshake.ScreenFunc)
}

// initLimits takes the transport's Guard (or makes one), applies the configured limits to it and
// hooks the swarm into the handshake and the dispatcher.
func (s *Swarm) initLimits() {
	s.lim.scorer = abuse.NewScorer(abuse.Config{})
	s.lim.recent = make(map[types.PeerID]time.Time)
	var g *handshake.Guard
	if o, ok := s.netTransport.(guardOwner); ok {
		g = o.Guard()
	}
	if g == nil {
		g = handshake.NewGuard(handshake.GuardConfig{})
	}
	if s.cfg != nil {
		n := s.cfg.Network
		g.SetPeerLimits(n.MaxPeersPerIP, n.MaxPeersPerSubnet, n.MaxIDsPerIP, n.MaxIDsPerSubnet)
	}
	s.lim.guard.Store(g)
	if sc, ok := s.netTransport.(screenSetter); ok {
		sc.SetScreen(s.screenPeer)
	}
	if r, ok := s.netTransport.(interface{ SetRetryAlways(bool) }); ok && s.cfg.IsServer() {
		r.SetRetryAlways(true)
	}
	if s.dispatcher != nil {
		s.dispatcher.SetReporter(s)
	}
	s.sessionManager.report = s.ReportPeer
}

// shareLimits gives another transport (WebSocket) the same Guard and screen.
func (s *Swarm) shareLimits(t any) {
	if gs, ok := t.(guardSetter); ok {
		gs.SetGuard(s.lim.guard.Load())
	}
	if sc, ok := t.(screenSetter); ok {
		sc.SetScreen(s.screenPeer)
	}
	if h, ok := t.(interface{ SetClientIPHeader(string) }); ok && s.cfg != nil {
		h.SetClientIPHeader(s.cfg.Network.WSClientIPHeader)
	}
}

// Guard is the per-address limiter shared by the swarm and its transports.
func (s *Swarm) Guard() *handshake.Guard { return s.lim.guard.Load() }

// SetTrustedPeers marks peers that are always let in and never banned for misbehaviour, such as
// the user's contacts; nil clears it.
func (s *Swarm) SetTrustedPeers(f func(types.PeerID) bool) {
	if f == nil {
		s.lim.trusted.Store(nil)
		return
	}
	s.lim.trusted.Store(&f)
}

func (s *Swarm) trusted(id types.PeerID) bool {
	f := s.lim.trusted.Load()
	return f != nil && (*f)(id)
}

// maxInbound is how many peers may be connected to this node by their own dial; 0: no limit.
func (s *Swarm) maxInbound() int {
	if s.cfg == nil {
		return DefaultMaxInboundPeers
	}
	switch n := s.cfg.Network.MaxInboundPeers; {
	case n < 0:
		return 0
	case n > 0:
		return n
	case s.cfg.IsServer():
		return DefaultServerMaxInboundPeers
	}
	return DefaultMaxInboundPeers
}

// IsKnownPeer reports whether the node has history with a peer: a trusted one, one it connected
// to before (peer store), or one that stayed connected for a while lately.
func (s *Swarm) IsKnownPeer(id types.PeerID) bool {
	if s.trusted(id) {
		return true
	}
	now := time.Now()
	s.lim.recentMu.Lock()
	until, ok := s.lim.recent[id]
	s.lim.recentMu.Unlock()
	if ok && now.Before(until) {
		return true
	}
	if p := s.GetPeer(id); p != nil && now.Sub(p.connectedAt) >= longLivedPeer {
		return true
	}
	if s.peerStore != nil {
		// 100 is what a connection writes; peers we only heard of get less (SaveDiscovered)
		if e, err := s.peerStore.GetPeer(id); err == nil && e.GetTrustScore() >= 100 {
			return true
		}
	}
	return false
}

// noteLongLived remembers a peer that stayed connected long enough to count as known.
func (s *Swarm) noteLongLived(p *Peer) {
	now := time.Now()
	if p.connectedAt.IsZero() || now.Sub(p.connectedAt) < longLivedPeer {
		return
	}
	s.lim.recentMu.Lock()
	defer s.lim.recentMu.Unlock()
	if len(s.lim.recent) >= maxRecent {
		for id, until := range s.lim.recent {
			if now.After(until) {
				delete(s.lim.recent, id)
			}
		}
		if len(s.lim.recent) >= maxRecent {
			s.lim.recent = make(map[types.PeerID]time.Time)
		}
	}
	s.lim.recent[p.id] = now.Add(knownFor)
}

// inboundCount is the number of connected peers that dialed this node.
func (s *Swarm) inboundCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, p := range s.activePeers {
		if !p.isOut {
			n++
		}
	}
	return n
}

// screenPeer runs inside an inbound handshake, before this node signs anything: a banned peer is
// refused, a stranger is refused while the node is full (only known peers may take a slot then),
// and a server asks strangers for a puzzle.
func (s *Swarm) screenPeer(pub types.PeerPublicKey) (uint32, bool) {
	id := types.PeerPubKeyToID(pub)
	if s.CheckOnBan(id) {
		return 0, true
	}
	known := s.IsKnownPeer(id)
	if max := s.maxInbound(); max > 0 && !known && !s.ThisIsActivePeer(id) && s.inboundCount() >= max {
		return 0, true
	}
	if s.cfg.IsServer() && !known {
		return handshake.StrangerPoWBits, false
	}
	return 0, false
}

// admitInbound decides whether an authenticated inbound peer may stay: within the limits of its
// address and subnet, and within the inbound cap. When the node is full a known peer takes the slot
// of the youngest inbound stranger; a stranger is turned away. On success release must be called
// once the peer is gone.
func (s *Swarm) admitInbound(ev network.NewConnEvent) (func(), error) {
	g := s.lim.guard.Load()
	release, err := g.AdmitPeer(ev.RemoteIP, ev.PeerID)
	if err != nil {
		if errors.Is(err, handshake.ErrTooManyIDs) {
			// one more fresh identity from an address that has shown too many
			s.lim.scorer.Report(ev.PeerID, ev.RemoteIP, abuse.Flood)
		}
		return nil, err
	}
	max := s.maxInbound()
	if max <= 0 || s.ThisIsActivePeer(ev.PeerID) {
		return release, nil
	}
	if s.inboundCount() < max {
		return release, nil
	}
	if s.IsKnownPeer(ev.PeerID) {
		if victim := s.evictionCandidate(); victim != nil {
			s.log().Debug("Swarm: inbound full, a known peer takes a stranger's slot", "evicted", victim.id.String(), "peer", ev.PeerID.String())
			s.RemovePeer(victim.id)
			return release, nil
		}
	}
	release()
	return nil, errInboundFull
}

// evictionCandidate is the inbound peer a known newcomer may replace: the youngest one that is not
// known itself. Long-lived and known peers are never pushed out.
func (s *Swarm) evictionCandidate() *Peer {
	s.mu.RLock()
	cands := make([]*Peer, 0, 16)
	for _, p := range s.activePeers {
		if !p.isOut {
			cands = append(cands, p)
		}
	}
	s.mu.RUnlock()
	var best *Peer
	for _, p := range cands {
		if best != nil && !p.connectedAt.After(best.connectedAt) {
			continue
		}
		if s.IsKnownPeer(p.id) {
			continue
		}
		best = p
	}
	return best
}

// ReportPeer takes a report of a peer breaking the protocol (abuse.Reporter). Points add up per
// peer and per address and fade with time; past the threshold the peer is banned for a while,
// and an address with several bad identities is turned away by the Guard. Trusted peers are not
// banned. Code that parses what peers send reports here.
func (s *Swarm) ReportPeer(id types.PeerID, r abuse.Reason) {
	var ip netip.Addr
	if p := s.GetPeer(id); p != nil {
		ip = p.ip
	}
	s.reportFrom(id, ip, r)
}

func (s *Swarm) reportFrom(id types.PeerID, ip netip.Addr, r abuse.Reason) {
	if s.trusted(id) {
		return
	}
	v := s.lim.scorer.Report(id, ip, r)
	if v.BanPeer {
		s.log().Warn("Swarm: banning a misbehaving peer", "peer", id.String(), "reason", r.String(), "for", v.PeerFor)
		s.BanPeerFor(id, v.PeerFor)
	}
	if v.BanAddr && ip.IsValid() {
		s.log().Warn("Swarm: banning an address whose peers misbehave", "addr", ip.String(), "reason", r.String(), "for", v.AddrFor)
		s.lim.guard.Load().Ban(ip, v.AddrFor)
		s.dropInboundFrom(ip)
	}
}

// BanPeerFor disconnects a peer and refuses it for d (0: for good); the ban is kept in the peer
// store, so it survives a restart.
func (s *Swarm) BanPeerFor(id types.PeerID, d time.Duration) {
	if s.peerStore != nil {
		_ = s.peerStore.BanPeerFor(id, d)
	}
	s.RemovePeer(id)
}

// dropInboundFrom disconnects every peer that dialed in from ip.
func (s *Swarm) dropInboundFrom(ip netip.Addr) {
	s.mu.RLock()
	var ids []types.PeerID
	for id, p := range s.activePeers {
		if !p.isOut && p.ip == ip {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()
	for _, id := range ids {
		s.RemovePeer(id)
	}
}

// acquireStream takes a slot for one stream handler of p; false: the peer, or the node as a
// whole, already runs as many as it may.
func (s *Swarm) acquireStream(p *Peer) bool {
	if s.lim.streams.Add(1) > maxStreamHandlers {
		s.lim.streams.Add(-1)
		return false
	}
	if p.pendingStreams.Add(1) > maxPendingStreamsPerPeer {
		p.pendingStreams.Add(-1)
		s.lim.streams.Add(-1)
		return false
	}
	return true
}
