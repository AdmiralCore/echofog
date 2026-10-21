package handshake

import (
	"net/netip"
	"sync"
	"time"
)

// AntiDDoS measures how many peers per second try to connect and turns that into the proof-of-work
// asked of every newcomer. While the node is calm nothing is asked; under a flood each handshake
// first costs the sender some CPU time (a hash puzzle) before the node spends its own on it.
type AntiDDoS struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets [windowSeconds]bucket
}

type bucket struct {
	second int64
	count  int
}

const (
	windowSeconds = 3

	// puzzle difficulty (leading zero bits) by arrival rate, in handshakes per second
	calmRate   = 25.0
	busyRate   = 100.0
	floodRate  = 400.0
	calmBits   = 0
	busyBits   = 16 // about 65 thousand hashes: a few milliseconds
	heavyBits  = 20 // about a million hashes: tens of milliseconds
	floodBits  = 22 // a few million hashes: a fraction of a second
	minPoWBits = 16 // when a node is configured to always ask

	// StrangerPoWBits is the puzzle a server asks of peers it does not know even when calm: about
	// 16 thousand hashes, a few milliseconds on a desktop and tens on a phone (see
	// TestStrangerPuzzleLatency), nothing to a real user and a cost per identity to a flood.
	StrangerPoWBits = 14
)

func NewAntiDDoS() *AntiDDoS { return &AntiDDoS{now: time.Now} }

// Note counts one incoming handshake attempt.
func (a *AntiDDoS) Note() {
	a.mu.Lock()
	defer a.mu.Unlock()
	sec := a.clock().Unix()
	b := &a.buckets[sec%windowSeconds]
	if b.second != sec {
		*b = bucket{second: sec}
	}
	b.count++
}

func (a *AntiDDoS) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// Rate is the average number of handshake attempts per second over the last few seconds.
func (a *AntiDDoS) Rate() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	sec := a.clock().Unix()
	total := 0
	for _, b := range a.buckets {
		if b.second > sec-windowSeconds && b.second <= sec {
			total += b.count
		}
	}
	return float64(total) / windowSeconds
}

// GetDifficulty is the number of leading zero bits asked of a newcomer right now; 0 means no puzzle.
func (a *AntiDDoS) GetDifficulty() uint32 {
	switch r := a.Rate(); {
	case r < calmRate:
		return calmBits
	case r < busyRate:
		return busyBits
	case r < floodRate:
		return heavyBits
	default:
		return floodBits
	}
}

// GuardConfig tunes the per-address limits; zero values mean the defaults.
type GuardConfig struct {
	Burst      int     // connection attempts an address may make in a burst
	Refill     float64 // attempts per second it earns back
	MaxActive  int     // handshakes of one address running at the same time
	FailLimit  int     // failed handshakes within FailWindow that earn a ban
	FailWindow time.Duration
	BanFor     time.Duration
	// CountLoopback applies the limits to 127.0.0.1 too (tests); by default local connections are
	// not limited.
	CountLoopback bool

	// Limits on authenticated inbound peers (see AdmitPeer); 0 means the default, negative no limit.
	MaxPeersPerIP     int // peers connected from one address at once
	MaxPeersPerSubnet int // from one /24 (IPv4) or /56 (IPv6) at once
	// Peer IDs cost nothing: an address may present only so many different ones within IDWindow.
	MaxIDsPerIP     int
	MaxIDsPerSubnet int
	IDWindow        time.Duration
}

func (c GuardConfig) withDefaults() GuardConfig {
	if c.Burst <= 0 {
		c.Burst = 30
	}
	if c.Refill <= 0 {
		c.Refill = 1
	}
	if c.MaxActive <= 0 {
		c.MaxActive = 8
	}
	if c.FailLimit <= 0 {
		c.FailLimit = 8
	}
	if c.FailWindow <= 0 {
		c.FailWindow = 10 * time.Minute
	}
	if c.BanFor <= 0 {
		c.BanFor = 15 * time.Minute
	}
	c.MaxPeersPerIP = orDefault(c.MaxPeersPerIP, DefaultMaxPeersPerIP)
	c.MaxPeersPerSubnet = orDefault(c.MaxPeersPerSubnet, DefaultMaxPeersPerSubnet)
	c.MaxIDsPerIP = orDefault(c.MaxIDsPerIP, DefaultMaxIDsPerIP)
	c.MaxIDsPerSubnet = orDefault(c.MaxIDsPerSubnet, DefaultMaxIDsPerSubnet)
	if c.IDWindow <= 0 {
		c.IDWindow = DefaultIDWindow
	}
	return c
}

// Guard limits what one remote address can do before it is authenticated: how fast it may open
// connections, how many handshakes it may run at once, and how many it may fail. An address that
// keeps failing (scanners, broken or hostile software) is banned for a while. Once a peer is
// authenticated the Guard also bounds how many peers, and how many different peer IDs, one address
// and its subnet hold (see AdmitPeer). All transports of a node share one Guard.
type Guard struct {
	cfg GuardConfig
	now func() time.Time

	mu    sync.Mutex
	ips   map[netip.Addr]*ipState
	addrs map[netip.Addr]*peerSet   // authenticated inbound peers by address
	subs  map[netip.Prefix]*peerSet // and by subnet
}

type ipState struct {
	tokens      float64
	last        time.Time
	active      int
	fails       int
	failsSince  time.Time
	bannedUntil time.Time
}

func NewGuard(cfg GuardConfig) *Guard {
	return &Guard{
		cfg:   cfg.withDefaults(),
		now:   time.Now,
		ips:   make(map[netip.Addr]*ipState),
		addrs: make(map[netip.Addr]*peerSet),
		subs:  make(map[netip.Prefix]*peerSet),
	}
}

const maxTrackedAddrs = 20000

func (g *Guard) exempt(ip netip.Addr) bool {
	return !g.cfg.CountLoopback && ip.IsLoopback()
}

// Allow says whether the address may start a handshake now. On success release must be called when
// the handshake is over.
func (g *Guard) Allow(ip netip.Addr) (release func(), ok bool) {
	ip = ip.Unmap()
	if !ip.IsValid() || g.exempt(ip) {
		return func() {}, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if len(g.ips) > maxTrackedAddrs {
		g.prune(now)
	}
	st := g.ips[ip]
	if st == nil {
		st = &ipState{tokens: float64(g.cfg.Burst), last: now}
		g.ips[ip] = st
	}
	if now.Before(st.bannedUntil) || g.fullLocked(ip) {
		return nil, false
	}
	st.tokens += now.Sub(st.last).Seconds() * g.cfg.Refill
	if st.tokens > float64(g.cfg.Burst) {
		st.tokens = float64(g.cfg.Burst)
	}
	st.last = now
	if st.tokens < 1 || st.active >= g.cfg.MaxActive {
		return nil, false
	}
	st.tokens--
	st.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			st.active--
			g.mu.Unlock()
		})
	}, true
}

// Fail records a handshake that did not succeed. Enough of them ban the address.
func (g *Guard) Fail(ip netip.Addr) {
	ip = ip.Unmap()
	if !ip.IsValid() || g.exempt(ip) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	st := g.ips[ip]
	if st == nil {
		st = &ipState{tokens: float64(g.cfg.Burst), last: now}
		g.ips[ip] = st
	}
	if st.fails == 0 || now.Sub(st.failsSince) > g.cfg.FailWindow {
		st.fails, st.failsSince = 0, now
	}
	st.fails++
	if st.fails >= g.cfg.FailLimit {
		st.bannedUntil = now.Add(g.cfg.BanFor)
		st.fails = 0
	}
}

// Success records a handshake that worked: the address is a proper peer, its failures are forgotten.
func (g *Guard) Success(ip netip.Addr) {
	ip = ip.Unmap()
	if !ip.IsValid() || g.exempt(ip) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.ips[ip]; st != nil {
		st.fails = 0
	}
}

// Banned reports whether the address is banned at the moment.
func (g *Guard) Banned(ip netip.Addr) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.ips[ip.Unmap()]
	return st != nil && g.now().Before(st.bannedUntil)
}

func (g *Guard) prune(now time.Time) {
	for ip, st := range g.ips {
		if st.active == 0 && now.After(st.bannedUntil) && now.Sub(st.last) > time.Hour {
			delete(g.ips, ip)
		}
	}
	if len(g.ips) > maxTrackedAddrs { // still too many: start over rather than grow without bound
		g.ips = make(map[netip.Addr]*ipState)
	}
}
