package clock

import (
	"slices"
	"sync"
	"time"
)

const (
	// maxPeerSamples is the number of authenticated peers whose offsets the clock keeps
	// (handshake.md §7: "the median offset to its last 5 authenticated peers").
	maxPeerSamples = 5
	// peerAgreement is how close two central samples must be to form a median when the
	// number of samples is even. Peer times travel in whole unix seconds, so honest
	// peers differ by at most about a second through receive-time jitter.
	peerAgreement = 2 * time.Second
	noPeerAfter   = time.Hour
	warnAfter     = 60 * time.Second
)

// PeerID is the authenticated identity of a peer that supplied a clock sample. The node
// passes the device ID it authenticated in the handshake; the clock treats it as opaque.
type PeerID [32]byte

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

type peerSample struct {
	peer   PeerID
	offset time.Duration
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

// AddPeerSample records the time reported by one authenticated peer. peerTime is the
// peer's HsInit.time or HsResp.time as received; the sample counts only after the peer
// has been authenticated (handshake.md §7).
func (c *Clock) AddPeerSample(peer PeerID, peerTime time.Time) {
	raw := c.now()
	c.mu.Lock()
	c.lastPeer = raw

	warn := false
	if !peerTime.IsZero() {
		offset := peerTime.Truncate(time.Second).Sub(raw.Truncate(time.Second))
		for i := range c.samples {
			if c.samples[i].peer == peer {
				c.samples = append(c.samples[:i], c.samples[i+1:]...)
				break
			}
		}
		c.samples = append(c.samples, peerSample{peer: peer, offset: offset})
		if len(c.samples) > maxPeerSamples {
			copy(c.samples, c.samples[len(c.samples)-maxPeerSamples:])
			c.samples = c.samples[:maxPeerSamples]
		}
		if offset, ok := medianOffset(c.samples); ok {
			warn = c.setOffsetLocked(offset)
		} else {
			// The two central samples disagree: fall back to the raw local clock
			// rather than keep a correction one sample supplied.
			warn = c.setOffsetLocked(0)
		}
	}
	offset := c.offset
	c.mu.Unlock()

	if warn && c.warn != nil {
		c.warn(offset)
	}
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
