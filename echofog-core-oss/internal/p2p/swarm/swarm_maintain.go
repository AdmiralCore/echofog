package swarm

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/p2p/peerstore"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	// MinPeers is how many connections the maintainer keeps: below it the node dials more.
	MinPeers = 8

	maintainBackoffMin = 15 * time.Second
	maintainBackoffMax = 5 * time.Minute

	historyDials        = 16 // known peers dialed per maintenance round
	tableDials          = 16 // routing table peers dialed per maintenance round
	startupHistoryDials = 32
	parallelDials       = 8
	candidateDialWait   = 5 * time.Second
)

// DialCandidate is a peer worth dialing and where.
type DialCandidate struct {
	ID   types.PeerID
	Addr string
}

// CandidateSource lists up to max more peers to dial when the node runs short (the DHT routing
// table is one).
type CandidateSource func(max int) []DialCandidate

// maintainState is the maintainer's backoff: rounds that do not bring the node back to enough
// peers come further apart, from 15 seconds to 5 minutes, with jitter so that nodes cut off
// together do not all dial the seeds at the same moment when they come back.
type maintainState struct {
	mu      sync.Mutex
	backoff time.Duration
	next    time.Time
}

// due reports whether a round should run now and, if so, schedules the next one.
func (m *maintainState) due(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.Before(m.next) {
		return false
	}
	switch {
	case m.backoff == 0:
		m.backoff = maintainBackoffMin
	case m.backoff < maintainBackoffMax:
		m.backoff = min(2*m.backoff, maintainBackoffMax)
	}
	jitter := 0.8 + 0.4*rand.Float64()
	m.next = now.Add(time.Duration(float64(m.backoff) * jitter))
	return true
}

// reset is for a node that has enough peers: the next shortage is answered at once.
func (m *maintainState) reset() {
	m.mu.Lock()
	m.backoff, m.next = 0, time.Time{}
	m.mu.Unlock()
}

// SetAddrPolicy sets which peer addresses this node hands on (peer exchange) and keeps.
func (s *Swarm) SetAddrPolicy(p network.AddrPolicy) { s.addrPolicy.Store(&p) }

// AddrPolicy is the policy set by SetAddrPolicy: public addresses only by default.
func (s *Swarm) AddrPolicy() network.AddrPolicy {
	if p := s.addrPolicy.Load(); p != nil {
		return *p
	}
	return network.AddrPolicy{}
}

// SetBootstrapNodes sets the nodes ("host:port/<peer id>") the maintainer dials when the node
// runs short of peers. Safe to call while the node runs.
func (s *Swarm) SetBootstrapNodes(list []string) {
	s.bootMu.Lock()
	s.bootstrap = append([]string(nil), list...)
	s.bootMu.Unlock()
}

// BootstrapNodes returns a copy of the list set by SetBootstrapNodes.
func (s *Swarm) BootstrapNodes() []string {
	s.bootMu.RLock()
	defer s.bootMu.RUnlock()
	return append([]string(nil), s.bootstrap...)
}

// AddCandidateSource adds a source of peers for the maintainer to dial.
func (s *Swarm) AddCandidateSource(f CandidateSource) {
	s.sourcesMu.Lock()
	s.candidates = append(s.candidates, f)
	s.sourcesMu.Unlock()
}

// OnPeerLearned registers a callback for every peer another node told us about (peer exchange),
// after its address passed the address policy.
func (s *Swarm) OnPeerLearned(f func(pub types.PeerPublicKey, addr string, caps handshake.Capability)) {
	s.sourcesMu.Lock()
	s.learned = append(s.learned, f)
	s.sourcesMu.Unlock()
}

// SetAutoDial sets whether the node dials peers it learned about on its own (peer exchange, the
// maintainer, DHT maintenance lookups); see config network.no_auto_dial.
func (s *Swarm) SetAutoDial(on bool) { s.autoDial.Store(on) }

// AutoDial reports whether the node dials peers it learned about on its own.
func (s *Swarm) AutoDial() bool { return s.autoDial.Load() }

// offersRelay tells whether announced capabilities include relaying or signaling. Unlike
// Peer.Relays it does not count a peer that announced nothing.
func offersRelay(c handshake.Capability) bool {
	return c.Has(handshake.CapRelay) || c.Has(handshake.CapSignal)
}

// NeedsPeers reports whether the node is short of peers: fewer than MinPeers, or none that
// relays or signals (without one, a node behind a NAT cannot be reached by anybody).
func (s *Swarm) NeedsPeers() bool {
	peers := s.GetAllPeers()
	if s.GuardModeActive() {
		// the node asks its guards for the rest of the network: it is short when none is up, or
		// none relays (the guards add themselves, see maintainGuards)
		if s.guardsUp() == 0 {
			return true
		}
		for _, p := range peers {
			if p.Signals() {
				return false
			}
		}
		return true
	}
	if len(peers) < MinPeers {
		return true
	}
	for _, p := range peers {
		if p.Signals() {
			return false
		}
	}
	return true
}

func (s *Swarm) relayingPeers() int {
	n := 0
	for _, p := range s.GetAllPeers() {
		if offersRelay(p.Caps()) {
			n++
		}
	}
	return n
}

// MaintainPeers keeps the node connected; the node calls it every few seconds. While the node is
// short of peers (NeedsPeers) it dials the bootstrap nodes, the peers it was connected to most
// recently and peers from the candidate sources, in rounds that back off from 15 seconds to 5
// minutes while they do not help. Returns how many peers a round dialed (0: no round ran).
func (s *Swarm) MaintainPeers() int {
	if !s.AutoDial() {
		return 0
	}
	if s.GuardModeActive() {
		s.maintainGuards()
	}
	if !s.NeedsPeers() {
		s.maint.reset()
		return 0
	}
	if !s.maint.due(time.Now()) {
		return 0
	}
	boot := s.BootstrapNodes()
	s.ConnectBootstrap(boot)

	cands := s.historyCandidates(historyDials)
	s.sourcesMu.RLock()
	sources := append([]CandidateSource(nil), s.candidates...)
	s.sourcesMu.RUnlock()
	for _, src := range sources {
		cands = append(cands, src(tableDials)...)
	}
	cands = s.dedupeCandidates(cands)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.dialCandidates(cands)
	}()
	return len(boot) + len(cands)
}

// historyCandidates are up to n stored peers, most recently seen first, that are not connected
// and whose retry after earlier failures is due.
func (s *Swarm) historyCandidates(n int) []DialCandidate {
	if s.peerStore == nil {
		return nil
	}
	now := time.Now()
	var out []DialCandidate
	for _, e := range s.peerStore.GetHistoryConnected(0) {
		if len(out) >= n {
			break
		}
		pub, err := types.ToPeerPubKey(e.GetPubKey())
		if err != nil || e.GetLastKnownAddr() == "" || !peerstore.DialDue(e, now) {
			continue
		}
		id := types.PeerPubKeyToID(pub)
		if id == s.selfID || s.ThisIsActivePeer(id) {
			continue
		}
		out = append(out, DialCandidate{ID: id, Addr: s.LearnPeerAddr(id, e.GetLastKnownAddr())})
	}
	return out
}

func (s *Swarm) dedupeCandidates(in []DialCandidate) []DialCandidate {
	seen := make(map[types.PeerID]bool, len(in))
	out := in[:0]
	for _, c := range in {
		if c.Addr == "" || c.ID == s.selfID || seen[c.ID] || s.ThisIsActivePeer(c.ID) || s.CheckOnBan(c.ID) {
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	return out
}

// dialCandidates dials each candidate, a few at a time, and notes the failures in the peer
// store: a peer that keeps failing is tried less and less often, then forgotten.
func (s *Swarm) dialCandidates(cands []DialCandidate) {
	sem := make(chan struct{}, parallelDials)
	var wg sync.WaitGroup
	for _, c := range cands {
		if s.ctx.Err() != nil {
			break
		}
		if s.ThisIsActivePeer(c.ID) {
			continue
		}
		if s.guardDialRefused(c.ID, c.Addr) {
			continue // guard mode: this one is learned about from the guards, not dialed
		}
		if max := s.maxConnections(); max > 0 && len(s.GetAllPeers()) >= max {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(c DialCandidate) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(s.ctx, candidateDialWait)
			defer cancel()
			if err := s.expectPeer(c.Addr, c.ID); err != nil {
				// not a failure of the peer: it is waiting for its camouflage SNI to reach us
				s.log().Debug("Swarm: known peer not dialed", "peer", c.ID.String(), "addr", c.Addr, "err", err)
				return
			}
			if err := s.dialEarly(ctx, c.Addr); err != nil {
				if s.ctx.Err() == nil && s.peerStore != nil {
					s.peerStore.RecordDialFailure(c.ID)
				}
				s.log().Debug("Swarm: known peer unreachable", "peer", c.ID.String(), "addr", c.Addr, "err", err)
			}
		}(c)
	}
	wg.Wait()
}

func (s *Swarm) maxConnections() int {
	if s.cfg == nil {
		return 0
	}
	return s.cfg.Network.MaxConnections
}
