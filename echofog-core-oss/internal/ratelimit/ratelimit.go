// Package ratelimit holds the small token buckets the relay and the exit use to cap what one peer
// (or one address) can take from this node.
package ratelimit

import (
	"net"
	"sync"
	"time"
)

// Bucket is a token bucket: it refills at rate tokens a second up to burst. A rate of 0 or less
// lets everything through. Safe for concurrent use.
type Bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// NewBucket returns a full bucket.
func NewBucket(rate, burst float64) *Bucket {
	return &Bucket{rate: rate, burst: burst, tokens: burst}
}

// SetRate changes the rate and burst; the tokens held are kept, up to the new burst.
func (b *Bucket) SetRate(rate, burst float64) {
	b.mu.Lock()
	b.rate, b.burst = rate, burst
	b.tokens = min(b.tokens, burst)
	b.mu.Unlock()
}

// Allow takes n tokens if the bucket holds them.
func (b *Bucket) Allow(n float64) bool { return b.AllowAt(n, time.Now()) }

// AllowAt is Allow at a given time.
func (b *Bucket) AllowAt(n float64, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate <= 0 {
		return true
	}
	b.refillLocked(now)
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// Wait takes n tokens, going into debt if it must, and returns how long the caller should wait
// before using them. A sender that sleeps that long keeps to the rate on average, whatever the
// size of its chunks.
func (b *Bucket) Wait(n float64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate <= 0 {
		return 0
	}
	b.refillLocked(time.Now())
	b.tokens -= n
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

func (b *Bucket) refillLocked(now time.Time) {
	if !b.last.IsZero() {
		if d := now.Sub(b.last).Seconds(); d > 0 {
			b.tokens = min(b.burst, b.tokens+d*b.rate)
		}
	}
	b.last = now
}

// Keyed holds one bucket per key, for at most max keys: when full, keys whose bucket has filled
// up again (they hold no state worth keeping) are dropped first, then an arbitrary half. A full
// table therefore never refuses a new key, it only forgets old ones.
type Keyed[K comparable] struct {
	mu    sync.Mutex
	rate  float64
	burst float64
	max   int
	m     map[K]*keyedEntry
}

type keyedEntry struct {
	tokens float64
	last   time.Time
}

// NewKeyed returns a per-key limiter: rate tokens a second up to burst for each key.
func NewKeyed[K comparable](rate, burst float64, max int) *Keyed[K] {
	if max <= 0 {
		max = 4096
	}
	return &Keyed[K]{rate: rate, burst: burst, max: max, m: make(map[K]*keyedEntry)}
}

// Allow takes n tokens from key's bucket if it holds them.
func (k *Keyed[K]) Allow(key K, n float64) bool { return k.AllowAt(key, n, time.Now()) }

// AllowAt is Allow at a given time.
func (k *Keyed[K]) AllowAt(key K, n float64, now time.Time) bool {
	if k == nil || k.rate <= 0 {
		return true
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	e := k.m[key]
	if e == nil {
		if len(k.m) >= k.max {
			k.shrinkLocked(now)
		}
		e = &keyedEntry{tokens: k.burst, last: now}
		k.m[key] = e
	} else if d := now.Sub(e.last).Seconds(); d > 0 {
		e.tokens = min(k.burst, e.tokens+d*k.rate)
		e.last = now
	}
	if e.tokens < n {
		return false
	}
	e.tokens -= n
	return true
}

// Prune forgets the keys whose bucket has filled up again.
func (k *Keyed[K]) Prune() {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.pruneLocked(time.Now())
	k.mu.Unlock()
}

// Len is how many keys are tracked.
func (k *Keyed[K]) Len() int {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}

func (k *Keyed[K]) pruneLocked(now time.Time) {
	for key, e := range k.m {
		if e.tokens+now.Sub(e.last).Seconds()*k.rate >= k.burst {
			delete(k.m, key)
		}
	}
}

func (k *Keyed[K]) shrinkLocked(now time.Time) {
	k.pruneLocked(now)
	if len(k.m) < k.max {
		return
	}
	drop := len(k.m) / 2
	for key := range k.m {
		if drop == 0 {
			break
		}
		delete(k.m, key)
		drop--
	}
}

// AddrKeys returns the keys a per-address limit counts addr under: ip is the address itself (an
// IPv6 /64, which one subscriber usually holds whole) and subnet its neighbourhood (an IPv4 /24 or
// an IPv6 /48), so many identities from one host or one rented block share a limit. addr is
// "host:port" or a bare host. Both are empty for loopback, private and link-local addresses and
// for anything unparsable: those cannot be forged from the Internet, and limiting them would
// throttle a LAN, a test cluster or a node behind a local reverse proxy as one client.
func AddrKeys(addr string) (ip, subnet string) {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	p := net.ParseIP(host)
	if p == nil || p.IsLoopback() || p.IsPrivate() || p.IsLinkLocalUnicast() || p.IsUnspecified() {
		return "", ""
	}
	if v4 := p.To4(); v4 != nil {
		return v4.String(), v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64", p.Mask(net.CIDRMask(48, 128)).String() + "/48"
}
