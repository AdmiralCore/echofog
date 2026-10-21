package handshake

import (
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Defaults of the limits on authenticated inbound peers (GuardConfig).
const (
	DefaultMaxPeersPerIP     = 16
	DefaultMaxPeersPerSubnet = 64
	DefaultMaxIDsPerIP       = 32
	DefaultMaxIDsPerSubnet   = 128
	DefaultIDWindow          = 10 * time.Minute
)

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// Errors of AdmitPeer.
var (
	ErrAddrFull   = errors.New("too many peers from this address")
	ErrSubnetFull = errors.New("too many peers from this subnet")
	ErrTooManyIDs = errors.New("too many peer identities from this address")
	ErrAddrBanned = errors.New("address is banned")
)

// peerSet is the peers connected from one address or subnet, and the IDs it showed lately.
type peerSet struct {
	active map[types.PeerID]int
	seen   map[types.PeerID]time.Time
}

func newPeerSet() *peerSet {
	return &peerSet{active: make(map[types.PeerID]int), seen: make(map[types.PeerID]time.Time)}
}

func (p *peerSet) prune(now time.Time, window time.Duration) {
	for id, at := range p.seen {
		if p.active[id] == 0 && now.Sub(at) > window {
			delete(p.seen, id)
		}
	}
}

// SubnetOf is the network a peer limit counts together with addr: its /24 for IPv4, its /56 for
// IPv6 (what one customer usually gets). Private and loopback addresses have none (false): a LAN
// or a container network is one operator, not an attack.
func SubnetOf(addr netip.Addr) (netip.Prefix, bool) {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() {
		return netip.Prefix{}, false
	}
	bits := 56
	if addr.Is4() {
		bits = 24
	}
	p, err := addr.Prefix(bits)
	return p, err == nil
}

// SetPeerLimits changes the limits on authenticated inbound peers (see GuardConfig): 0 keeps the
// default, negative removes the limit.
func (g *Guard) SetPeerLimits(perIP, perSubnet, idsPerIP, idsPerSubnet int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cfg.MaxPeersPerIP = orDefault(perIP, DefaultMaxPeersPerIP)
	g.cfg.MaxPeersPerSubnet = orDefault(perSubnet, DefaultMaxPeersPerSubnet)
	g.cfg.MaxIDsPerIP = orDefault(idsPerIP, DefaultMaxIDsPerIP)
	g.cfg.MaxIDsPerSubnet = orDefault(idsPerSubnet, DefaultMaxIDsPerSubnet)
}

// SetClock replaces the time source (tests).
func (g *Guard) SetClock(now func() time.Time) {
	g.mu.Lock()
	g.now = now
	g.mu.Unlock()
}

// fullLocked: the address or its subnet holds as many peers as it may, so a new handshake from it
// would be refused after authentication anyway. g.mu is held.
func (g *Guard) fullLocked(ip netip.Addr) bool {
	if ps := g.addrs[ip]; ps != nil && g.cfg.MaxPeersPerIP > 0 && len(ps.active) >= g.cfg.MaxPeersPerIP {
		return true
	}
	if sub, ok := SubnetOf(ip); ok {
		if ps := g.subs[sub]; ps != nil && g.cfg.MaxPeersPerSubnet > 0 && len(ps.active) >= g.cfg.MaxPeersPerSubnet {
			return true
		}
	}
	return false
}

// Refuses reports, without counting an attempt, whether a connection from the address would be
// turned away: it is banned or holds all the peers it may. Cheap enough to ask before any
// cryptography is done for the connection.
func (g *Guard) Refuses(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || g.exempt(ip) {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.ips[ip]; st != nil && g.now().Before(st.bannedUntil) {
		return true
	}
	return g.fullLocked(ip)
}

// Ban turns the address away for d (misbehaviour seen after authentication).
func (g *Guard) Ban(ip netip.Addr, d time.Duration) {
	ip = ip.Unmap()
	if !ip.IsValid() || g.exempt(ip) || d <= 0 {
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
	if until := now.Add(d); until.After(st.bannedUntil) {
		st.bannedUntil = until
	}
}

// AdmitPeer counts an authenticated inbound peer against its address and subnet. It fails when
// the address is banned, holds MaxPeersPerIP peers already, its subnet MaxPeersPerSubnet, or when
// the address (or subnet) showed too many different peer IDs within IDWindow. A peer ID already
// connected from the address is always let in again (a reconnect). On success release must be
// called once the peer is gone.
func (g *Guard) AdmitPeer(ip netip.Addr, id types.PeerID) (release func(), err error) {
	ip = ip.Unmap()
	if !ip.IsValid() || g.exempt(ip) {
		return func() {}, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if st := g.ips[ip]; st != nil && now.Before(st.bannedUntil) {
		return nil, ErrAddrBanned
	}
	if len(g.addrs) > maxTrackedAddrs || len(g.subs) > maxTrackedAddrs {
		g.prunePeers(now)
	}
	ap := g.addrs[ip]
	if ap == nil {
		ap = newPeerSet()
		g.addrs[ip] = ap
	}
	var sp *peerSet
	if sub, ok := SubnetOf(ip); ok {
		if sp = g.subs[sub]; sp == nil {
			sp = newPeerSet()
			g.subs[sub] = sp
		}
	}
	if ap.active[id] == 0 {
		if g.cfg.MaxPeersPerIP > 0 && len(ap.active) >= g.cfg.MaxPeersPerIP {
			return nil, ErrAddrFull
		}
		if sp != nil && sp.active[id] == 0 && g.cfg.MaxPeersPerSubnet > 0 && len(sp.active) >= g.cfg.MaxPeersPerSubnet {
			return nil, ErrSubnetFull
		}
		ap.prune(now, g.cfg.IDWindow)
		if _, seen := ap.seen[id]; !seen && g.cfg.MaxIDsPerIP > 0 && len(ap.seen) >= g.cfg.MaxIDsPerIP {
			return nil, ErrTooManyIDs
		}
		if sp != nil {
			sp.prune(now, g.cfg.IDWindow)
			if _, seen := sp.seen[id]; !seen && g.cfg.MaxIDsPerSubnet > 0 && len(sp.seen) >= g.cfg.MaxIDsPerSubnet {
				return nil, ErrTooManyIDs
			}
		}
	}
	ap.active[id]++
	ap.seen[id] = now
	if sp != nil {
		sp.active[id]++
		sp.seen[id] = now
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			// the ID stays in seen until the window has passed: leaving and coming back as
			// another identity still counts
			left := g.now()
			if ap.active[id]--; ap.active[id] <= 0 {
				delete(ap.active, id)
				ap.seen[id] = left
			}
			if sp != nil {
				if sp.active[id]--; sp.active[id] <= 0 {
					delete(sp.active, id)
					sp.seen[id] = left
				}
			}
		})
	}, nil
}

// PeersFrom is the number of authenticated inbound peers connected from the address now.
func (g *Guard) PeersFrom(ip netip.Addr) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ps := g.addrs[ip.Unmap()]; ps != nil {
		return len(ps.active)
	}
	return 0
}

func (g *Guard) prunePeers(now time.Time) {
	for ip, ps := range g.addrs {
		ps.prune(now, g.cfg.IDWindow)
		if len(ps.active) == 0 && len(ps.seen) == 0 {
			delete(g.addrs, ip)
		}
	}
	for sub, ps := range g.subs {
		ps.prune(now, g.cfg.IDWindow)
		if len(ps.active) == 0 && len(ps.seen) == 0 {
			delete(g.subs, sub)
		}
	}
}
