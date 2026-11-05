package exit

import (
	"sync"
	"time"
)

// Default per-client-identity limits of services.md §7.
const (
	DefaultTCPPerSecond     = 20
	DefaultTCPTargetsPerMin = 256
	DefaultUDPSessions      = 64
	DefaultUDPTargets       = 256
	DefaultUDPPacketsPerSec = 2000
)

// tokenBucket is a rate limiter over an injected clock. It refills at rate tokens a second and
// holds at most one second of burst, so a client cannot save up an unbounded allowance.
type tokenBucket struct {
	rate   float64
	tokens float64
	at     time.Time
}

func newTokenBucket(rate float64, at time.Time) tokenBucket {
	return tokenBucket{rate: rate, tokens: rate, at: at}
}

// take removes one token if it is there.
func (b *tokenBucket) take(now time.Time) bool {
	if b.rate <= 0 {
		return true
	}
	if d := now.Sub(b.at).Seconds(); d > 0 {
		b.tokens = min(b.rate, b.tokens+d*b.rate)
		b.at = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// tcpLimiter applies the per-identity connection rate and distinct-target rate of §7.
type tcpLimiter struct {
	mu      sync.Mutex
	perSec  float64
	targets int
	window  time.Duration
	byNode  map[[32]byte]*tcpNode
}

type tcpNode struct {
	conns tokenBucket
	seen  map[string]time.Time
}

func newTCPLimiter(perSec, targets int) *tcpLimiter {
	return &tcpLimiter{perSec: float64(perSec), targets: targets, window: time.Minute,
		byNode: map[[32]byte]*tcpNode{}}
}

// allow counts one new TCP connection to target for node. It returns false when either the
// connection rate or the distinct-target rate is exceeded.
func (l *tcpLimiter) allow(now time.Time, node [32]byte, target string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.byNode[node]
	if n == nil {
		n = &tcpNode{conns: newTokenBucket(l.perSec, now), seen: map[string]time.Time{}}
		l.byNode[node] = n
	}
	if !n.conns.take(now) {
		return false
	}
	for t, at := range n.seen {
		if now.Sub(at) > l.window {
			delete(n.seen, t)
		}
	}
	if _, ok := n.seen[target]; ok {
		n.seen[target] = now
		return true
	}
	if len(n.seen) >= l.targets {
		return false
	}
	n.seen[target] = now
	return true
}

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
func (l *packetLimiter) allow(now time.Time, node [32]byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.byNode[node]
	if b == nil {
		tb := newTokenBucket(l.rate, now)
		b = &tb
		l.byNode[node] = b
	}
	return b.take(now)
}
