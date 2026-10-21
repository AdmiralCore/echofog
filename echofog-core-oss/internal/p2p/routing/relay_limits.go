package routing

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/ratelimit"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Built-in limits of a relay; config.RelayServerConfig overrides each of them (0 keeps these).
const (
	defaultCircuitsPerPeer = 16
	defaultCircuitsPerIP   = 32 // one IPv4 /24 or IPv6 /48 gets twice this
	defaultReservedPct     = 25
	defaultCircuitIdle     = 5 * time.Minute
	defaultCircuitIdleKB   = 32
	defaultCircuitMaxMB    = 4096
	defaultPeerMBPerDay    = 16384
	defaultPeerMbps        = 50

	// establishedAfter: a peer connected for this long is not new, and may use the circuit slots
	// kept back from fresh identities.
	establishedAfter = 10 * time.Minute

	// relay requests (store-and-forward messages) one origin may send a second, and at once
	relayReqRate  = 20
	relayReqBurst = 60
	// a "mailbox full" answer costs a signature: at most one every few seconds per peer
	fullReplyRate  = 0.2
	fullReplyBurst = 2
	// how many peers the per-peer limiters remember before forgetting idle ones
	limiterPeers = 8192

	limitLogEvery = 30 * time.Second
)

// relayLimits are the configured limits with the defaults filled in. 0 here means no limit.
type relayLimits struct {
	maxCircuits     int
	newPeerCircuits int // circuits peers that are not established may hold together
	perPeer         int
	perIP           int
	perSubnet       int
	idle            time.Duration
	idleBytes       uint64
	circuitBytes    uint64
	peerDayBytes    uint64
	peerRate        float64 // bytes a second
	nodeRate        float64
}

// pick: v when set, def for 0, and 0 (no limit) for a negative v.
func pick(v, def int64) int64 {
	switch {
	case v < 0:
		return 0
	case v == 0:
		return def
	}
	return v
}

func mbpsToBytes(mbps int64) float64 { return float64(mbps) * 1e6 / 8 }

func limitsOf(cfg config.RelayServerConfig) relayLimits {
	l := relayLimits{
		maxCircuits:  int(pick(int64(cfg.MaxCircuits), config.DefaultRelayMaxCircuits)),
		perPeer:      int(pick(int64(cfg.MaxCircuitsPerPeer), defaultCircuitsPerPeer)),
		perIP:        int(pick(int64(cfg.MaxCircuitsPerIP), defaultCircuitsPerIP)),
		idle:         time.Duration(pick(int64(cfg.CircuitIdleSeconds), int64(defaultCircuitIdle/time.Second))) * time.Second,
		idleBytes:    uint64(pick(int64(cfg.CircuitIdleKB), defaultCircuitIdleKB)) << 10,
		circuitBytes: uint64(pick(cfg.CircuitMaxMB, defaultCircuitMaxMB)) << 20,
		peerDayBytes: uint64(pick(cfg.PeerMaxMBPerDay, defaultPeerMBPerDay)) << 20,
		peerRate:     mbpsToBytes(pick(int64(cfg.PeerMaxMbps), defaultPeerMbps)),
		nodeRate:     mbpsToBytes(max(0, int64(cfg.MaxMbps))),
	}
	l.perSubnet = 2 * l.perIP
	if l.maxCircuits > 0 {
		pct := min(100, pick(int64(cfg.ReservedCircuitsPct), defaultReservedPct))
		l.newPeerCircuits = l.maxCircuits - l.maxCircuits*int(pct)/100
		if l.newPeerCircuits <= 0 {
			l.newPeerCircuits = -1 // everything reserved: fresh peers get nothing
		}
	}
	return l
}

// rateBurst: a second of traffic, and never less than 1 MiB, so a video key frame passes whole.
func rateBurst(rate float64) float64 { return max(rate, 1<<20) }

// peerUse is what the circuits one peer opened have carried: today's bytes against its daily
// budget, and a token bucket for its speed.
type peerUse struct {
	mu     sync.Mutex
	day    time.Time
	bytes  uint64
	bucket *ratelimit.Bucket
}

// exhausted: the peer has used its whole daily budget.
func (u *peerUse) exhausted(lim relayLimits, now time.Time) bool {
	if lim.peerDayBytes == 0 {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return !u.day.Before(now.UTC().Truncate(24*time.Hour)) && u.bytes >= lim.peerDayBytes
}

type frameVerdict int

const (
	framePass       frameVerdict = iota
	frameDrop                    // over a speed limit: the frame is dropped, like a congested link would
	frameOverBudget              // over a byte budget: the circuit is closed
)

// chargeFrame accounts n bytes of a normal circuit against its budgets and speed limits.
func (rs *RelayServer) chargeFrame(c *Circuit, n uint64, lim relayLimits, now time.Time) frameVerdict {
	if lim.circuitBytes > 0 && atomic.LoadUint64(&c.BytesRouted)+n > lim.circuitBytes {
		return frameOverBudget
	}
	if u := c.use; u != nil {
		u.mu.Lock()
		if today := now.UTC().Truncate(24 * time.Hour); u.day.Before(today) {
			u.day, u.bytes = today, 0
		}
		if lim.peerDayBytes > 0 && u.bytes+n > lim.peerDayBytes {
			u.mu.Unlock()
			return frameOverBudget
		}
		if !u.bucket.AllowAt(float64(n), now) {
			u.mu.Unlock()
			return frameDrop
		}
		u.bytes += n
		u.mu.Unlock()
	}
	if !rs.nodeBucket.AllowAt(float64(n), now) {
		return frameDrop
	}
	return framePass
}

// useOf returns the peer's usage record, creating it.
func (rs *RelayServer) useOf(peer types.PeerID, lim relayLimits) *peerUse {
	rs.useMu.Lock()
	defer rs.useMu.Unlock()
	u := rs.use[peer]
	if u == nil {
		u = &peerUse{bucket: ratelimit.NewBucket(lim.peerRate, rateBurst(lim.peerRate))}
		rs.use[peer] = u
	}
	return u
}

// pruneUse forgets the usage of peers that hold no circuit and used nothing today; past
// limiterPeers it forgets every peer without a circuit.
func (rs *RelayServer) pruneUse(holding map[types.PeerID]bool, now time.Time) {
	today := now.UTC().Truncate(24 * time.Hour)
	rs.useMu.Lock()
	defer rs.useMu.Unlock()
	crowded := len(rs.use) > limiterPeers
	for id, u := range rs.use {
		if holding[id] {
			continue
		}
		u.mu.Lock()
		stale := u.day.Before(today) || u.bytes == 0
		u.mu.Unlock()
		if stale || crowded {
			delete(rs.use, id)
		}
	}
}

// established: the peer has been connected to this node for establishedAfter or more.
func (rs *RelayServer) established(peer types.PeerID, now time.Time) bool {
	rs.peersMu.Lock()
	defer rs.peersMu.Unlock()
	at, ok := rs.connectedAt[peer]
	return ok && now.Sub(at) >= establishedAfter
}

// addrKeys returns the per-IP and per-subnet keys of a connected peer ("" when not limited).
func (rs *RelayServer) addrKeys(peer types.PeerID) (string, string) {
	if rs.addrOf == nil {
		return "", ""
	}
	return ratelimit.AddrKeys(rs.addrOf(peer))
}

// admitLocked says why a new circuit from peer may not open ("" when it may). Caller holds
// circuitsMu (read or write).
func (rs *RelayServer) admitLocked(peer types.PeerID, ipKey, netKey string, established bool, lim relayLimits) string {
	total, byPeer, byIP, byNet := len(rs.circuits), 0, 0, 0
	for _, c := range rs.circuits {
		if c.PeerA == peer {
			byPeer++
		}
		if ipKey != "" && c.ipKey == ipKey {
			byIP++
		}
		if netKey != "" && c.netKey == netKey {
			byNet++
		}
	}
	switch {
	case lim.maxCircuits > 0 && total >= lim.maxCircuits:
		return "circuit table full"
	case !established && lim.newPeerCircuits != 0 && total >= max(0, lim.newPeerCircuits):
		return "remaining circuits kept for established peers"
	case lim.perPeer > 0 && byPeer >= lim.perPeer:
		return "per-peer circuit cap"
	case lim.perIP > 0 && byIP >= lim.perIP:
		return "per-IP circuit cap"
	case lim.perSubnet > 0 && byNet >= lim.perSubnet:
		return "per-subnet circuit cap"
	}
	return ""
}

// warnLimited logs a refusal at most once per limitLogEvery for each reason, with how many were
// refused since the last line: a flood of them must not flood the log.
func (rs *RelayServer) warnLimited(reason string, args ...any) {
	rs.logMu.Lock()
	rs.logSkipped[reason]++
	if time.Since(rs.logLast[reason]) < limitLogEvery {
		rs.logMu.Unlock()
		return
	}
	n := rs.logSkipped[reason]
	rs.logLast[reason], rs.logSkipped[reason] = time.Now(), 0
	rs.logMu.Unlock()
	rs.logger.Warn("RelayServer: refused: "+reason, append(args, "times", n)...)
}
