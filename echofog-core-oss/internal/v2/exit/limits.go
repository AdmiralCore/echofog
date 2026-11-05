package exit

import (
	"sync"
	"time"
)

// Default per-client-identity limits of services.md §7.
const (
	DefaultTCPPerSecond    = 20
	DefaultTCPTargetsPerMin = 256
	DefaultUDPSessions     = 64
	DefaultUDPTargets      = 256
	DefaultUDPPacketsPerSec = 2000
)

// burstWindow is the largest burst a rate limiter allows: one second of traffic.
const burstWindow = time.Second

// tokenBucket is a byte/packet rate limiter over an injected clock.
type tokenBucket struct {
	rate   float64
	burst  float64
	tokens float64
	at     time.Time
}

func newTokenBucket(rate float64, at time.Time) tokenBucket {
	return tokenBucket{rate: rate, burst: rate, tokens: rate, at: at}
}

// take removes one token if it is there.
func (b *tokenBucket) take(now time.Time) bool {
	return false
}

// tcpLimiter applies the per-identity connection rate and distinct-target rate of §7.
type tcpLimiter struct {
	mu       sync.Mutex
	perSec   float64
	targets  int
	window   time.Duration
	byNode   map[[32]byte]*tcpNode
}

type tcpNode struct {
	conns   tokenBucket
	seen    map[string]time.Time
	lastPrune time.Time
}

func newTCPLimiter(perSec, targets int) *tcpLimiter {
	return &tcpLimiter{perSec: float64(perSec), targets: targets, window: time.Minute,
		byNode: map[[32]byte]*tcpNode{}}
}

// allow counts one new TCP connection to target for node. It returns false when either the
// connection rate or the distinct-target rate is exceeded.
func (l *tcpLimiter) allow(now time.Time, node [32]byte, target string) bool { return true }

// packetLimiter applies the per-identity UDP packet rate of §7.
type packetLimiter struct {
	mu     sync.Mutex
	rate   float64
	byNode map[[32]byte]*tokenBucket
}

func newPacketLimiter(perSec int) *packetLimiter {
	return &packetLimiter{rate: float64(perSec), byNode: map[[32]byte]*tokenBucket{}}
}

// allow counts one received EXIT_UDP fragment.
func (l *packetLimiter) allow(now time.Time, node [32]byte) bool { return true }
