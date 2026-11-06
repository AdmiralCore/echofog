package clock

import (
	"net/netip"
	"slices"
	"sync"
	"time"
)

const (
	// maxPeerSamples is the number of networks whose samples the clock keeps (handshake.md §7).
	maxPeerSamples = 5
	// minNetworks is the least number of distinct /16 networks a median needs.
	minNetworks = 3
	// maxPeerOffset bounds the offset peers can set, from the raw clock; maxPeerStep bounds one
	// update. A larger correction needs the CDN check (handshake.md §7, audit M1 B-2).
	maxPeerOffset = 600 * time.Second
	maxPeerStep   = 120 * time.Second
	// peerAgreement is how close two central samples must be to form a median when the
	// number of samples is even. Peer times travel in whole unix seconds, so honest
	// peers differ by at most about a second through receive-time jitter.
	peerAgreement = 2 * time.Second
	noPeerAfter   = time.Hour
	warnAfter     = 60 * time.Second
)

// Config configures a Clock.
type Config struct {
	// Now is the raw, uncorrected local wall clock. Nil means time.Now. Tests inject a
	// fake clock; production passes time.Now and lets the offset do the correction.
	Now func() time.Time
	// HTTP performs the CDN requests. It must use browser-shaped TLS and, in anonymous
	// mode, the node's proxy (02-architecture §3.1: node.Host.HTTP). Nil disables the
	// CDN check: MaybeCheckCDN reports ErrNoHTTPClient.
	HTTP HTTPClient
	// Endpoints replaces the DefaultEndpoints list. It must contain at least 3 absolute
	// https URLs (registries.md §12: a node needs at least 3 answers).
	Endpoints []string
	// Warn is called when the applied offset exceeds 60 s, once per crossing.
	Warn func(offset time.Duration)
}

// Clock is the node's corrected clock. The zero value is not usable; call New.
type Clock struct {
	now       func() time.Time
	http      HTTPClient
	endpoints []string
	warn      func(time.Duration)

	mu        sync.Mutex
	offset    time.Duration
	samples   []peerSample
	lastPeer  time.Time
	lastCheck time.Time
	checking  bool
	warned    bool
}

// netKey names the network of a sample: the /16 of an IPv4 address, the /32 of an IPv6 one.
type netKey [5]byte

type peerSample struct {
	net    netKey
	offset time.Duration
}

func keyOf(a netip.Addr) (netKey, bool) {
	a = a.Unmap()
	switch {
	case a.Is4():
		b := a.As4()
		return netKey{4, b[0], b[1]}, true
	case a.Is6():
		b := a.As16()
		return netKey{6, b[0], b[1], b[2], b[3]}, true
	}
	return netKey{}, false
}

// New returns a Clock. Without Config.Now it uses time.Now; without Config.Endpoints it
// uses the registries.md §12 list.
func New(cfg Config) (*Clock, error) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	endpoints := cfg.Endpoints
	if endpoints == nil {
		endpoints = DefaultEndpoints()
	}
	if len(endpoints) < minCDNAnswers {
		return nil, ErrTooFewEndpoints
	}
	if len(endpoints) > maxEndpoints {
		return nil, ErrTooManyEndpoints
	}
	copied := make([]string, len(endpoints))
	for i, endpoint := range endpoints {
		if !validEndpoint(endpoint) {
			return nil, ErrInvalidEndpoint
		}
		copied[i] = endpoint
	}
	return &Clock{
		now:       now,
		http:      cfg.HTTP,
		endpoints: copied,
		warn:      cfg.Warn,
	}, nil
}

// Now returns the corrected local time: the raw clock plus the current offset.
func (c *Clock) Now() time.Time {
	raw := c.now()
	c.mu.Lock()
	offset := c.offset
	c.mu.Unlock()
	return raw.Add(offset)
}

// Unix returns the corrected time in unix seconds, the form used by HsInit.time,
// HsResp.time and the L0 ts field.
func (c *Clock) Unix() uint64 { return uint64(c.Now().Unix()) }

// Offset returns the current correction added to the raw local clock.
func (c *Clock) Offset() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset
}

// AddOutgoingSample records the time reported by the peer of one session this node dialed
// (HsResp.time, authenticated). Incoming sessions give no sample: anyone can open one (handshake.md
// section 7). At most one sample counts per /16 network (IPv6: /32), the median is used only when at
// least 3 networks gave samples, it never moves the offset by more than 120 s at once, and the
// offset never exceeds 600 s from the raw clock; only the CDN check can set a larger one.
func (c *Clock) AddOutgoingSample(remote netip.Addr, peerTime time.Time) {
	raw := c.now()
	c.mu.Lock()
	c.lastPeer = raw

	warn := false
	if key, ok := keyOf(remote); ok && !peerTime.IsZero() {
		offset := peerTime.Truncate(time.Second).Sub(raw.Truncate(time.Second))
		for i := range c.samples {
			if c.samples[i].net == key {
				c.samples = append(c.samples[:i], c.samples[i+1:]...)
				break
			}
		}
		c.samples = append(c.samples, peerSample{net: key, offset: offset})
		if len(c.samples) > maxPeerSamples {
			copy(c.samples, c.samples[len(c.samples)-maxPeerSamples:])
			c.samples = c.samples[:maxPeerSamples]
		}
		if len(c.samples) >= minNetworks {
			target, ok := medianOffset(c.samples)
			if !ok {
				// The two central samples disagree: no estimate, drift back to the raw clock.
				target = 0
			}
			target = clampDuration(target, maxPeerOffset)
			next := c.offset + clampDuration(target-c.offset, maxPeerStep)
			warn = c.setOffsetLocked(next)
		}
	}
	offset := c.offset
	c.mu.Unlock()

	if warn && c.warn != nil {
		c.warn(offset)
	}
}

func clampDuration(d, limit time.Duration) time.Duration {
	if d > limit {
		return limit
	}
	if d < -limit {
		return -limit
	}
	return d
}

// setOffsetLocked applies offset and reports whether a new warning must be raised.
// The caller must hold c.mu.
func (c *Clock) setOffsetLocked(offset time.Duration) bool {
	c.offset = offset
	if absDuration(offset) <= warnAfter {
		c.warned = false
		return false
	}
	if c.warned {
		return false
	}
	c.warned = true
	return true
}

// medianOffset returns the median peer offset. With an even number of samples the
// midpoint of the two central values is used only when they agree; otherwise there is
// no correction (see the package documentation).
func medianOffset(samples []peerSample) (time.Duration, bool) {
	if len(samples) == 0 {
		return 0, false
	}
	offsets := make([]time.Duration, len(samples))
	for i := range samples {
		offsets[i] = samples[i].offset
	}
	slices.Sort(offsets)

	n := len(offsets)
	if n%2 == 1 {
		return offsets[n/2], true
	}
	low, high := offsets[n/2-1], offsets[n/2]
	if high-low > peerAgreement {
		return 0, false
	}
	return low + (high-low)/2, true
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
