package exit_gateway

import (
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/ratelimit"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Limits bound what peers can take from this exit. 0 in a field means no limit.
type Limits struct {
	MaxStreams int // TCP tunnels and UDP sessions at once, everybody together
	PerPeer    int // ... for one peer
	PerIP      int // ... for the peers of one IP address (an IPv6 /64)
	UDPPerPeer int // UDP sessions (exit sockets) one peer holds

	Idle        time.Duration // a TCP tunnel that moved nothing either way for this long is closed
	MaxLifetime time.Duration // a TCP tunnel is closed after this long

	// On a free exit: bytes a second one peer, and the peers of one IP together, may use.
	FreePeerRate float64
	FreeIPRate   float64

	// On a free exit: bytes one peer, and the peers of one IP together, may take in a day (UTC),
	// and the whole exit in a month (UTC). 0: no cap, which is the default.
	FreePeerDaily int64
	FreeIPDaily   int64
	FreeMonthly   int64
}

// Built-in exit limits; config.ExitConfig overrides each of them (0 keeps these).
const (
	defaultExitStreams        = 4096
	defaultExitStreamsPerPeer = 256
	defaultExitStreamsPerIP   = 512
	defaultExitUDPPerPeer     = 32
	defaultExitIdle           = 5 * time.Minute
	defaultFreePeerMbps       = 50
	defaultFreeIPMbps         = 100

	// account and voucher requests: a few a second per peer is plenty for a client
	queryRate  = 2
	queryBurst = 10
	// voucher offers come more often (one per slice of paid traffic), and a known vault is cheap
	offerRate  = 10
	offerBurst = 30
	// quotaPeers bounds the per-peer free quota table
	quotaPeers = 10000
)

// DefaultLimits are the limits of an exit nobody configured.
func DefaultLimits() Limits { return LimitsFromConfig(config.ExitConfig{}) }

// pickLimit: v when set, def for 0, and 0 (no limit) for a negative v.
func pickLimit(v, def int64) int64 {
	switch {
	case v < 0:
		return 0
	case v == 0:
		return def
	}
	return v
}

// LimitsFromConfig fills the defaults into an ExitConfig.
func LimitsFromConfig(c config.ExitConfig) Limits {
	mbps := func(v, def int) float64 { return float64(pickLimit(int64(v), int64(def))) * 1e6 / 8 }
	return Limits{
		MaxStreams:   int(pickLimit(int64(c.MaxStreams), defaultExitStreams)),
		PerPeer:      int(pickLimit(int64(c.MaxStreamsPerPeer), defaultExitStreamsPerPeer)),
		PerIP:        int(pickLimit(int64(c.MaxStreamsPerIP), defaultExitStreamsPerIP)),
		UDPPerPeer:   int(pickLimit(int64(c.MaxUDPPerPeer), defaultExitUDPPerPeer)),
		Idle:         time.Duration(pickLimit(int64(c.IdleSeconds), int64(defaultExitIdle/time.Second))) * time.Second,
		MaxLifetime:  time.Duration(max(0, c.MaxLifetimeSeconds)) * time.Second,
		FreePeerRate: mbps(c.FreePeerMbps, defaultFreePeerMbps),
		FreeIPRate:   mbps(c.FreeIPMbps, defaultFreeIPMbps),

		FreePeerDaily: max(0, c.FreePeerMBPerDay) << 20,
		FreeIPDaily:   max(0, c.FreeIPMBPerDay) << 20,
		FreeMonthly:   max(0, c.FreeGBPerMonth) << 30,
	}
}

// SetLimits changes the exit's limits. Streams already open keep their speed limits until they end.
func (e *ExitService) SetLimits(l Limits) {
	e.slots.mu.Lock()
	e.limits = l
	e.slots.mu.Unlock()
	e.shaper.mu.Lock()
	e.shaper.peerRate, e.shaper.ipRate = l.FreePeerRate, l.FreeIPRate
	e.shaper.mu.Unlock()
}

// CurrentLimits returns the exit's limits.
func (e *ExitService) CurrentLimits() Limits {
	e.slots.mu.Lock()
	defer e.slots.mu.Unlock()
	return e.limits
}

// SetAddrOf tells the exit how to find a peer's remote address, for the per-IP limits; nil (the
// default) leaves them off.
func (e *ExitService) SetAddrOf(f func(types.PeerID) string) {
	e.slots.mu.Lock()
	e.addrOf = f
	e.slots.mu.Unlock()
}

// streamSlots counts the streams the exit serves, so one peer, one address or everybody together
// stays within the caps. Its maps hold only peers and addresses with something open.
type streamSlots struct {
	mu     sync.Mutex
	total  int
	byPeer map[types.PeerID]int
	byIP   map[string]int
	udp    map[types.PeerID]int
}

// acquire takes a stream slot for peer, or says why there is none. release gives it back.
func (e *ExitService) acquire(peer types.PeerID, udp bool) (release func(), ok bool) {
	e.slots.mu.Lock()
	l, addrOf := e.limits, e.addrOf
	e.slots.mu.Unlock()
	var ip string
	if addrOf != nil {
		ip, _ = ratelimit.AddrKeys(addrOf(peer))
	}

	s := &e.slots
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byPeer == nil {
		s.byPeer, s.byIP, s.udp = map[types.PeerID]int{}, map[string]int{}, map[types.PeerID]int{}
	}
	switch {
	case l.MaxStreams > 0 && s.total >= l.MaxStreams,
		l.PerPeer > 0 && s.byPeer[peer] >= l.PerPeer,
		ip != "" && l.PerIP > 0 && s.byIP[ip] >= l.PerIP,
		udp && l.UDPPerPeer > 0 && s.udp[peer] >= l.UDPPerPeer:
		return nil, false
	}
	s.total++
	s.byPeer[peer]++
	if ip != "" {
		s.byIP[ip]++
	}
	if udp {
		s.udp[peer]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.total--
			dec := func(m map[types.PeerID]int, k types.PeerID) {
				if m[k]--; m[k] <= 0 {
					delete(m, k)
				}
			}
			dec(s.byPeer, peer)
			if udp {
				dec(s.udp, peer)
			}
			if ip != "" {
				if s.byIP[ip]--; s.byIP[ip] <= 0 {
					delete(s.byIP, ip)
				}
			}
		})
	}, true
}

// Streams returns how many streams the exit serves right now.
func (e *ExitService) Streams() int {
	e.slots.mu.Lock()
	defer e.slots.mu.Unlock()
	return e.slots.total
}

// shaper keeps the free exit fair: every peer, and every address, gets its own token bucket, so
// one of them (or many identities behind one address) cannot take the whole uplink. A bucket is
// shared by all of a peer's streams and lives while any of them is open.
type shaper struct {
	mu       sync.Mutex
	peerRate float64
	ipRate   float64
	peers    map[types.PeerID]*sharedBucket
	ips      map[string]*sharedBucket
}

type sharedBucket struct {
	*ratelimit.Bucket
	users int
}

// pace is what one stream holds: the buckets it draws from, nil ones being unlimited.
type pace struct {
	peer, ip *ratelimit.Bucket
}

// wait takes n bytes from the buckets and returns how long to wait before passing them on.
func (p *pace) wait(n int) time.Duration {
	if p == nil {
		return 0
	}
	var d time.Duration
	if p.peer != nil {
		d = p.peer.Wait(float64(n))
	}
	if p.ip != nil {
		d = max(d, p.ip.Wait(float64(n)))
	}
	return d
}

// allow takes n bytes if the buckets hold them: datagrams over the share are dropped, not delayed.
func (p *pace) allow(n int) bool {
	if p == nil {
		return true
	}
	if p.peer != nil && !p.peer.Allow(float64(n)) {
		return false
	}
	return p.ip == nil || p.ip.Allow(float64(n))
}

// throttle sleeps as long as the stream's buckets say.
func (p *pace) throttle(n int) {
	if d := p.wait(n); d > 0 {
		time.Sleep(d)
	}
}

// paceFor returns the buckets a stream of peer draws from, and the function that lets them go.
// Only a free exit shapes, and never the peers on its free list: those are the operator's.
func (e *ExitService) paceFor(peer types.PeerID) (*pace, func()) {
	e.policyMu.Lock()
	shaped := e.policy.PricePerMB <= 0 && !e.freePeers[peer]
	e.policyMu.Unlock()
	if !shaped {
		return nil, func() {}
	}
	e.slots.mu.Lock()
	addrOf := e.addrOf
	e.slots.mu.Unlock()
	var ip string
	if addrOf != nil {
		ip, _ = ratelimit.AddrKeys(addrOf(peer))
	}

	sh := &e.shaper
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.peers == nil {
		sh.peers, sh.ips = map[types.PeerID]*sharedBucket{}, map[string]*sharedBucket{}
	}
	p := &pace{}
	var pb, ib *sharedBucket
	if sh.peerRate > 0 {
		if pb = sh.peers[peer]; pb == nil {
			pb = &sharedBucket{Bucket: ratelimit.NewBucket(sh.peerRate, paceBurst(sh.peerRate))}
			sh.peers[peer] = pb
		}
		pb.users++
		p.peer = pb.Bucket
	}
	if ip != "" && sh.ipRate > 0 {
		if ib = sh.ips[ip]; ib == nil {
			ib = &sharedBucket{Bucket: ratelimit.NewBucket(sh.ipRate, paceBurst(sh.ipRate))}
			sh.ips[ip] = ib
		}
		ib.users++
		p.ip = ib.Bucket
	}
	var once sync.Once
	return p, func() {
		once.Do(func() {
			sh.mu.Lock()
			defer sh.mu.Unlock()
			if pb != nil {
				if pb.users--; pb.users <= 0 {
					delete(sh.peers, peer)
				}
			}
			if ib != nil {
				if ib.users--; ib.users <= 0 {
					delete(sh.ips, ip)
				}
			}
		})
	}
}

// paceBurst: a quarter second of traffic, at least one relay buffer, so a stream is never held
// up on a chunk it cannot pass at all.
func paceBurst(rate float64) float64 { return max(rate/4, relayBufSize) }

// allowQuery: account and voucher requests from one peer are answered a few a second at most.
func (e *ExitService) allowQuery(peer types.PeerID) bool {
	return e.queries.Allow(peer, 1)
}

// AllowQuery is allowQuery for the node's other exit requests (the exit directory).
func (e *ExitService) AllowQuery(peer types.PeerID) bool { return e.allowQuery(peer) }
