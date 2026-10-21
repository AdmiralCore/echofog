// Package abuse keeps a misbehaviour score per peer and per address. Code that catches a peer
// breaking the protocol reports it; scores fade with time, and one that climbs over the threshold
// gets the peer (or every identity behind the address) banned for a while. Peer IDs cost nothing
// to make, so every report also counts against the address it came from.
package abuse

import (
	"math"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Reason is what a peer did wrong; each weighs differently.
type Reason int

const (
	// BadFrame: a frame or message that does not parse, or a stream that breaks the protocol.
	BadFrame Reason = iota + 1
	// BadSignature: a signed envelope whose signature or origin does not hold.
	BadSignature
	// Flood: the peer hit a rate or concurrency limit (queue share, streams, sessions).
	Flood
	// Refused: a request the node refused (no session slot, over quota); cheap on its own,
	// suspicious when it never stops.
	Refused
	// BadHandshake: an authenticated peer failed a later check of its identity.
	BadHandshake
)

func (r Reason) String() string {
	switch r {
	case BadFrame:
		return "bad_frame"
	case BadSignature:
		return "bad_signature"
	case Flood:
		return "flood"
	case Refused:
		return "refused"
	case BadHandshake:
		return "bad_handshake"
	}
	return "unknown"
}

// Weight is how many points one report adds; Threshold points ban.
func (r Reason) Weight() float64 {
	switch r {
	case BadFrame:
		return 10
	case BadSignature:
		return 25
	case Flood:
		return 3
	case Refused:
		return 2
	case BadHandshake:
		return 34
	}
	return 1
}

// Reporter takes reports of misbehaving peers. The swarm implements it; anything that handles
// what peers send can report into it.
type Reporter interface {
	ReportPeer(peer types.PeerID, r Reason)
}

// ReporterFunc adapts a function to Reporter.
type ReporterFunc func(peer types.PeerID, r Reason)

func (f ReporterFunc) ReportPeer(peer types.PeerID, r Reason) {
	if f != nil {
		f(peer, r)
	}
}

// Nop drops every report.
var Nop Reporter = ReporterFunc(nil)

// Config tunes the scorer; zero values mean the defaults.
type Config struct {
	Threshold float64       // points that earn a ban (default 100)
	HalfLife  time.Duration // how fast points fade (default 10 minutes)
	PeerBan   time.Duration // how long a peer is banned (default 1 hour)
	AddrBan   time.Duration // how long an address is banned (default 15 minutes)
	// AddrThreshold is the score of an address that bans it: several identities behind one address
	// add up (default 2x Threshold, so one bad peer alone does not ban its neighbours behind a NAT)
	AddrThreshold float64
	// CountLoopback scores 127.0.0.1 too (tests); by default local addresses are never banned.
	CountLoopback bool
}

func (c Config) withDefaults() Config {
	if c.Threshold <= 0 {
		c.Threshold = 100
	}
	if c.HalfLife <= 0 {
		c.HalfLife = 10 * time.Minute
	}
	if c.PeerBan <= 0 {
		c.PeerBan = time.Hour
	}
	if c.AddrBan <= 0 {
		c.AddrBan = 15 * time.Minute
	}
	if c.AddrThreshold <= 0 {
		c.AddrThreshold = 2 * c.Threshold
	}
	return c
}

// Verdict is what a report led to.
type Verdict struct {
	BanPeer bool
	BanAddr bool
	PeerFor time.Duration
	AddrFor time.Duration
}

// Scorer adds up reports per peer and per address, with exponential decay.
type Scorer struct {
	cfg Config
	now func() time.Time

	mu    sync.Mutex
	peers map[types.PeerID]*score
	addrs map[netip.Addr]*score
}

type score struct {
	points    float64
	at        time.Time
	lastFlood time.Time
}

// floodGap: a peer that keeps hitting a limit is reported on every hit, but scores one Flood per
// floodGap; a burst is one offence, only a flood that goes on adds up.
const floodGap = time.Second

// slack: reports that add up to exactly a threshold reach it, though a few microseconds of decay
// passed between them.
const slack = 0.01

const maxTracked = 20000

func NewScorer(cfg Config) *Scorer {
	return &Scorer{
		cfg:   cfg.withDefaults(),
		now:   time.Now,
		peers: make(map[types.PeerID]*score),
		addrs: make(map[netip.Addr]*score),
	}
}

// SetClock replaces the time source (tests).
func (s *Scorer) SetClock(now func() time.Time) { s.now = now }

func (s *Scorer) decayed(sc *score, now time.Time) float64 {
	dt := now.Sub(sc.at)
	if dt <= 0 {
		return sc.points
	}
	return sc.points * math.Exp2(-float64(dt)/float64(s.cfg.HalfLife))
}

func (s *Scorer) add(sc *score, w float64, now time.Time) float64 {
	sc.points = s.decayed(sc, now) + w
	sc.at = now
	return sc.points
}

// Report adds one misbehaviour of peer, seen from addr (may be the zero Addr), and says whether
// it crossed a threshold. A ban resets the score that earned it.
func (s *Scorer) Report(peer types.PeerID, addr netip.Addr, r Reason) Verdict {
	addr = addr.Unmap()
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.peers) > maxTracked || len(s.addrs) > maxTracked {
		s.prune(now)
	}
	var v Verdict
	sc := s.peers[peer]
	if sc == nil {
		sc = &score{at: now}
		s.peers[peer] = sc
	}
	if r == Flood {
		if !sc.lastFlood.IsZero() && now.Sub(sc.lastFlood) < floodGap {
			return v
		}
		sc.lastFlood = now
	}
	if s.add(sc, r.Weight(), now) >= s.cfg.Threshold-slack {
		v.BanPeer, v.PeerFor = true, s.cfg.PeerBan
		delete(s.peers, peer)
	}
	if addr.IsValid() && (s.cfg.CountLoopback || !addr.IsLoopback()) {
		as := s.addrs[addr]
		if as == nil {
			as = &score{at: now}
			s.addrs[addr] = as
		}
		if s.add(as, r.Weight(), now) >= s.cfg.AddrThreshold-slack {
			v.BanAddr, v.AddrFor = true, s.cfg.AddrBan
			delete(s.addrs, addr)
		}
	}
	return v
}

// PeerScore is the current score of a peer (diagnostics, tests).
func (s *Scorer) PeerScore(peer types.PeerID) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc := s.peers[peer]; sc != nil {
		return s.decayed(sc, s.now())
	}
	return 0
}

// AddrScore is the current score of an address.
func (s *Scorer) AddrScore(addr netip.Addr) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc := s.addrs[addr.Unmap()]; sc != nil {
		return s.decayed(sc, s.now())
	}
	return 0
}

func (s *Scorer) prune(now time.Time) {
	for id, sc := range s.peers {
		if s.decayed(sc, now) < 1 {
			delete(s.peers, id)
		}
	}
	for a, sc := range s.addrs {
		if s.decayed(sc, now) < 1 {
			delete(s.addrs, a)
		}
	}
	if len(s.peers) > maxTracked {
		s.peers = make(map[types.PeerID]*score)
	}
	if len(s.addrs) > maxTracked {
		s.addrs = make(map[netip.Addr]*score)
	}
}
