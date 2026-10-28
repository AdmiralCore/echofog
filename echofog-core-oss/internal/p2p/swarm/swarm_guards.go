package swarm

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Guard mode. A user's node in a censored country does not build a mesh of its own across the
// border: every link it dials there is a flow the censor can look at, and the links among ordinary
// nodes are noise, which is what a GFW-style heuristic flags. Instead the node keeps links to two
// or three camouflaged guard servers (REALITY over QUIC, or browser TLS over TCP) and takes the
// rest of the network from them: peer exchange runs over those links, and a DHT lookup asks the
// guards, which walk the DHT on the node's behalf (pkg dht, proxied lookups).
//
// Guards are chosen from the known servers with a camouflage SNI, kept for weeks (like Tor's
// entry guards, so a node does not walk through the whole list of servers over time), replaced
// when they stay gone, and one failing never leaves the node without a network: the others carry
// on, and with all of them gone the node may dial any camouflaged server until it has guards again.
// Peers on the local network, and in the home region where region_aware_obfs allows it, are dialed
// directly either way; a country that cannot be placed counts as across the border.

// ErrGuardDialRefused: the node is in guard mode, the peer is across the border and is not a
// guard. The dial is not made; the lookup or the maintainer goes on with the guards.
var ErrGuardDialRefused = errors.New("swarm: not dialing across the border outside the guards")

const (
	guardsWanted = 3 // guards held at once
	guardsMax    = guardsWanted + 2

	// A guard stays for guardLifeMin plus a random span, then makes room for another one. Rarely,
	// on purpose: every new guard is one more server that sees this node's address.
	guardLifeMin  = 30 * 24 * time.Hour
	guardLifeSpan = 30 * 24 * time.Hour

	// guardDownAfter: a guard not connected for this long is down, and another is added beside it
	// (the downed one stays in the list: the outage may be a short one). guardDropAfter: down this
	// long while the node still has other links, so the network is up and the guard is the one
	// missing, it is dropped for good.
	guardDownAfter = 90 * time.Second
	guardDropAfter = 20 * time.Minute

	guardDialWait    = 6 * time.Second
	guardRedialMin   = 5 * time.Second
	guardRedialMax   = time.Minute
	guardsStorageKey = "guards:v1"
)

type guard struct {
	id      types.PeerID
	addr    string // plain host:port
	added   time.Time
	expires time.Time
	lastOK  time.Time // last time it was seen connected, or when it was added
	fails   int
	next    time.Time // not dialed before this
	dialing bool
}

// GuardInfo is a guard as the status of the node shows it.
type GuardInfo struct {
	ID        types.PeerID
	Addr      string
	Connected bool
	Added     time.Time
	Expires   time.Time
}

type guardSet struct {
	mu     sync.Mutex
	list   []*guard
	loaded bool
}

// GuardModeActive reports whether this node holds guards: it is not in the server role and
// anti_dpi.guard_mode is "on", or "auto" with obfuscation on (the default).
func (s *Swarm) GuardModeActive() bool {
	if s == nil || s.cfg == nil || s.cfg.IsServer() {
		return false
	}
	switch s.cfg.AntiDPI.GuardMode {
	case config.GuardOff:
		return false
	case config.GuardOn:
		return true
	}
	return s.cfg.AntiDPI.Obfs == config.ObfsNode
}

// Guards lists the guards, connected or not.
func (s *Swarm) Guards() []GuardInfo {
	s.guards.mu.Lock()
	defer s.guards.mu.Unlock()
	s.loadGuardsLocked()
	out := make([]GuardInfo, 0, len(s.guards.list))
	for _, g := range s.guards.list {
		out = append(out, GuardInfo{ID: g.id, Addr: g.addr, Connected: s.ThisIsActivePeer(g.id), Added: g.added, Expires: g.expires})
	}
	return out
}

// GuardsWanted is how many guards the node aims to hold at once.
func (s *Swarm) GuardsWanted() int { return guardsWanted }

// RefreshGuards runs a guard round now: after the mode changed, it starts holding guards (or lets
// the kept ones go on the next rounds). Safe when guard mode is off.
func (s *Swarm) RefreshGuards() {
	if s == nil {
		return
	}
	s.maintainGuards()
}

// IsGuard reports whether id is one of this node's guards.
func (s *Swarm) IsGuard(id types.PeerID) bool {
	s.guards.mu.Lock()
	defer s.guards.mu.Unlock()
	s.loadGuardsLocked()
	for _, g := range s.guards.list {
		if g.id == id {
			return true
		}
	}
	return false
}

// guardsUp is how many guards are connected now.
func (s *Swarm) guardsUp() int {
	s.guards.mu.Lock()
	defer s.guards.mu.Unlock()
	s.loadGuardsLocked()
	n := 0
	for _, g := range s.guards.list {
		if s.ThisIsActivePeer(g.id) {
			n++
		}
	}
	return n
}

// guardDialRefused reports whether a dial the node starts on its own (the maintainer, peer
// exchange, a DHT lookup) must not be made: in guard mode, to a peer across the border that is not
// a guard. With no guard up, camouflaged servers are allowed (the way back to a network; a dial to
// one is REALITY, never noise).
func (s *Swarm) guardDialRefused(id types.PeerID, addr string) bool {
	if !s.GuardModeActive() || s.IsGuard(id) {
		return false
	}
	info := network.ParseAddrInfo(addr)
	if !s.acrossBorder(info.Addr) {
		return false
	}
	if s.guardsUp() == 0 && (info.SNI != "" || s.GetPeerSNI(id) != "") {
		return false
	}
	return true
}

// DialMeshPeer dials a peer the node has no particular reason to talk to — one a DHT lookup was
// referred to — unless guard mode keeps it from crossing the border. A dial the user asked for (a
// contact, a call, a relay or an exit) goes through AddPotentialPeerWithCtx instead.
func (s *Swarm) DialMeshPeer(ctx context.Context, id types.PeerID, addr string) error {
	if s.guardDialRefused(id, addr) {
		return ErrGuardDialRefused
	}
	return s.AddPotentialPeerWithCtx(ctx, id, addr)
}

// ConnectBootstrap dials the bootstrap nodes at start and when the node is short of peers. In
// guard mode they are candidates for the guards, and only guards are dialed; without it every
// entry is.
func (s *Swarm) ConnectBootstrap(list []string) {
	if !s.GuardModeActive() {
		s.Bootstrap(list)
		return
	}
	var rest []string
	for _, e := range list {
		info := network.ParseAddrInfo(e)
		if info.Named && (info.SNI != "" || s.GetPeerSNI(info.ID) != "") {
			s.learnBootstrapFacts(info)
		} else {
			rest = append(rest, e) // no peer ID or no camouflage: it cannot be a guard, dial it as before
		}
	}
	if len(rest) > 0 {
		s.Bootstrap(rest)
	}
	s.maintainGuards()
}

func (s *Swarm) learnBootstrapFacts(info network.AddrInfo) {
	if info.SNI != "" {
		s.SetPeerSNI(info.ID, info.SNI)
	}
	if info.Server {
		s.SetPeerServer(info.ID, true)
	}
	s.MarkExplicitPeer(info.ID)
}

type guardCandidate struct {
	id   types.PeerID
	addr string
}

// eligibleGuard: a server with a camouflage SNI, not a user's node and not banned.
func (s *Swarm) eligibleGuard(id types.PeerID) bool {
	if id == s.selfID || s.CheckOnBan(id) || s.GetPeerSNI(id) == "" || s.isClientPeer(id) {
		return false
	}
	return s.IsServerPeer(id) || s.isExplicitPeer(id)
}

// guardCandidates are the servers a guard can be picked from, with where to dial them: the
// bootstrap nodes, the servers connected now and those that were connected before.
func (s *Swarm) guardCandidates() []guardCandidate {
	seen := map[types.PeerID]bool{}
	var out []guardCandidate
	add := func(id types.PeerID, addr string) {
		if addr == "" || seen[id] || !s.eligibleGuard(id) {
			return
		}
		seen[id] = true
		out = append(out, guardCandidate{id, network.ParseAddrInfo(addr).Addr})
	}
	for _, e := range s.BootstrapNodes() {
		if info := network.ParseAddrInfo(e); info.Named {
			add(info.ID, e)
		}
	}
	for _, p := range s.GetAllPeers() {
		addr := p.Addr()
		if addr == "" {
			addr = p.RemoteAddr()
		}
		add(p.ID(), addr)
	}
	if s.peerStore != nil {
		for _, e := range s.peerStore.GetHistoryConnected(0) {
			pub, err := types.ToPeerPubKey(e.GetPubKey())
			if err != nil {
				continue
			}
			add(types.PeerPubKeyToID(pub), e.GetLastKnownAddr())
		}
	}
	return out
}

// maintainGuards is one round: update which guards are up, drop the expired and the long gone,
// add guards while fewer than wanted are up, dial those that are not connected. The node calls it
// every few seconds.
func (s *Swarm) maintainGuards() {
	now := time.Now()
	for _, c := range s.planGuards(now, s.guardCandidates()) {
		s.dialGuard(c)
	}
}

// planGuards is the bookkeeping of a round, apart from the network: it changes the guard list and
// returns the guards that are to be dialed now.
func (s *Swarm) planGuards(now time.Time, cands []guardCandidate) []*guard {
	s.guards.mu.Lock()
	defer s.guards.mu.Unlock()
	s.loadGuardsLocked()
	changed := false

	up := func(g *guard) bool { return s.ThisIsActivePeer(g.id) }
	othersUp := len(s.GetAllPeers()) // links of any kind: the proof that this node's own network works
	kept := s.guards.list[:0]
	dropped := map[types.PeerID]bool{} // not taken back in the same round
	for _, g := range s.guards.list {
		switch {
		case !now.Before(g.expires), s.CheckOnBan(g.id), !s.eligibleGuardOrKept(g):
			changed, dropped[g.id] = true, true // its time is up (connected or not), or it is no use
			continue
		case up(g):
			g.lastOK, g.fails = now, 0
		case now.Sub(g.lastOK) > guardDropAfter && othersUp > 0:
			changed, dropped[g.id] = true, true
			continue
		}
		kept = append(kept, g)
	}
	s.guards.list = kept

	healthy := func() int {
		n := 0
		for _, g := range s.guards.list {
			if up(g) || now.Sub(g.lastOK) <= guardDownAfter {
				n++
			}
		}
		return n
	}
	if healthy() < guardsWanted && len(s.guards.list) < guardsMax {
		inList := map[types.PeerID]bool{}
		for _, g := range s.guards.list {
			inList[g.id] = true
		}
		var pool []guardCandidate
		for _, c := range cands {
			if !inList[c.id] && !dropped[c.id] {
				pool = append(pool, c)
			}
		}
		rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
		// connected servers first (a link that already works), then spread over networks: a
		// second guard in the /16 of a first one adds little
		sort.SliceStable(pool, func(i, j int) bool {
			ci, cj := s.ThisIsActivePeer(pool[i].id), s.ThisIsActivePeer(pool[j].id)
			return ci && !cj
		})
		for pass := 0; pass < 2; pass++ {
			for _, c := range pool {
				if healthy() >= guardsWanted || len(s.guards.list) >= guardsMax || inList[c.id] {
					continue
				}
				if pass == 0 && s.sameNetworkAsGuard(c.addr) {
					continue
				}
				life := guardLifeMin + time.Duration(rand.Int64N(int64(guardLifeSpan)))
				s.guards.list = append(s.guards.list, &guard{id: c.id, addr: c.addr, added: now, expires: now.Add(life), lastOK: now})
				inList[c.id] = true
				changed = true
			}
		}
	}
	var dial []*guard
	for _, g := range s.guards.list {
		if up(g) || g.dialing || now.Before(g.next) {
			continue
		}
		g.dialing = true
		dial = append(dial, g)
	}
	if changed {
		s.saveGuardsLocked()
	}
	return dial
}

// eligibleGuardOrKept is true while the guard is a server we know: a guard that turned out to be
// a user's node (it announced no server role) is dropped.
func (s *Swarm) eligibleGuardOrKept(g *guard) bool {
	return !s.isClientPeer(g.id)
}

func (s *Swarm) sameNetworkAsGuard(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return false
	}
	for _, g := range s.guards.list {
		h, _, err := net.SplitHostPort(g.addr)
		if err != nil {
			continue
		}
		if o := net.ParseIP(h).To4(); o != nil && o[0] == ip[0] && o[1] == ip[1] {
			return true
		}
	}
	return false
}

// dialGuard dials a guard in the background and notes how it went.
func (s *Swarm) dialGuard(g *guard) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		addr := resolveHostPort(s.ctx, g.addr)
		ctx, cancel := context.WithTimeout(s.ctx, guardDialWait)
		defer cancel()
		err := s.expectPeer(addr, g.id)
		if err == nil {
			err = s.dialEarly(ctx, addr)
		}
		s.guards.mu.Lock()
		g.dialing = false
		if err != nil && !s.ThisIsActivePeer(g.id) && !errors.Is(err, ErrSelfAddr) {
			g.fails++
			back := guardRedialMin << min(g.fails-1, 4)
			g.next = time.Now().Add(min(back, guardRedialMax))
		}
		s.guards.mu.Unlock()
		if err != nil && s.ctx.Err() == nil {
			s.log().Debug("Swarm: guard not reachable", "guard", g.id.String()[:12], "addr", g.addr, "err", err)
		}
	}()
}

// persistence: one record, JSON, so a restart keeps the same guards

type guardRecord struct {
	ID      string `json:"id"`
	Addr    string `json:"addr"`
	Added   int64  `json:"added"`
	Expires int64  `json:"expires"`
}

func (s *Swarm) saveGuardsLocked() {
	if s.storage == nil {
		return
	}
	recs := make([]guardRecord, 0, len(s.guards.list))
	for _, g := range s.guards.list {
		recs = append(recs, guardRecord{ID: hex.EncodeToString(g.id[:]), Addr: g.addr, Added: g.added.Unix(), Expires: g.expires.Unix()})
	}
	if b, err := json.Marshal(recs); err == nil {
		_ = s.storage.Set([]byte(guardsStorageKey), b)
	}
}

func (s *Swarm) loadGuardsLocked() {
	if s.guards.loaded {
		return
	}
	s.guards.loaded = true
	if s.storage == nil {
		return
	}
	b, err := s.storage.Get([]byte(guardsStorageKey))
	if err != nil || len(b) == 0 {
		return
	}
	var recs []guardRecord
	if json.Unmarshal(b, &recs) != nil {
		return
	}
	now := time.Now()
	for _, r := range recs {
		raw, err := hex.DecodeString(r.ID)
		if err != nil || len(raw) != 32 || len(s.guards.list) >= guardsMax {
			continue
		}
		var id types.PeerID
		copy(id[:], raw)
		s.guards.list = append(s.guards.list, &guard{id: id, addr: r.Addr, added: time.Unix(r.Added, 0), expires: time.Unix(r.Expires, 0), lastOK: now})
	}
}

// ServesUsers: this node is a server, so users' nodes may use it as their guard.
func (s *Swarm) ServesUsers() bool { return s != nil && s.cfg != nil && s.cfg.IsServer() }

// IsUserPeer reports whether the peer is known to be a user's node (not a server).
func (s *Swarm) IsUserPeer(id types.PeerID) bool { return s.isClientPeer(id) }

// ErrUserLinkViaRelay: a user's node is across the border, and the link to it is not made direct:
// the traffic goes through a camouflaged relay (a circuit), because a direct link between two
// users is noise or a hole punch, which a censor can flag.
var ErrUserLinkViaRelay = errors.New("swarm: a user's node across the border is reached through a relay, not directly")

// UserLinkViaRelay reports whether the node must not link directly (a dial, a hole punch) to the
// peer at addr ("" if no address is known): in guard mode, a peer that is not a camouflaged server
// and not one the user listed (an invitation, a bootstrap node), at an address across the border.
// With no address known the peer counts as across the border: unknown is across. The traffic to
// such a peer goes through relays, in the first place the node's guards.
func (s *Swarm) UserLinkViaRelay(id types.PeerID, addr string) bool {
	if !s.GuardModeActive() || s.isExplicitPeer(id) || s.IsGuard(id) {
		return false
	}
	if s.GetPeerSNI(id) != "" || s.IsServerPeer(id) {
		return false // a server: REALITY with its SNI, or held back by the noise rule
	}
	info := network.ParseAddrInfo(addr)
	if info.SNI != "" {
		return false
	}
	if info.Addr == "" {
		return true
	}
	return s.acrossBorder(info.Addr)
}

// GuardDialRefused is guardDialRefused for the node layer: a dial it starts on its own to a peer it
// has no reason to talk to.
func (s *Swarm) GuardDialRefused(id types.PeerID, addr string) bool {
	return s.guardDialRefused(id, addr)
}
