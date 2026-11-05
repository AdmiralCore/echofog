package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// BindCarrier binds one more carrier to the session with PATH_BIND (wire.md §8.2). It returns
// the new path ID. The caller owns c until this call; on any failure the carrier is closed.
func (e *Established) BindCarrier(ctx context.Context, c mux.Carrier, kind mux.PathKind) (uint64, error) {
	if e == nil || e.Session == nil || c == nil {
		if c != nil {
			_ = c.Close()
		}
		return 0, fmt.Errorf("%w: no session or carrier", ErrConfig)
	}
	id, err := e.Session.BindPath(ctx, mux.NewGuard(c), kind)
	if err != nil {
		_ = c.Close()
		return 0, err
	}
	return id, nil
}

// AbandonPath tells the peer that a path is no longer used and closes it (wire.md §8.3). The
// last live path cannot be abandoned; closing the session ends it instead.
func (e *Established) AbandonPath(id, code uint64) error {
	if e == nil || e.Session == nil {
		return fmt.Errorf("%w: no session", ErrConfig)
	}
	return e.Session.AbandonPath(id, code)
}

// Paths returns the live path IDs and the active path.
func (e *Established) Paths() (ids []uint64, active uint64, ok bool) {
	if e == nil || e.Session == nil {
		return nil, 0, false
	}
	return e.Session.Paths()
}

// LivePaths returns the number of live paths of the session.
func (e *Established) LivePaths() int {
	ids, _, _ := e.Paths()
	return len(ids)
}

// HasFeature reports whether a feature was negotiated.
func (e *Established) HasFeature(id uint16) bool {
	return e != nil && e.Session != nil && e.Session.HasFeature(id)
}

// Pair is the key of the local carrier policy (H11, H15): one destination address and one SNI.
// Two carriers at most go to one pair; after a penalty the pair is backed off.
type Pair struct {
	Host string
	SNI  string
}

// PathPolicy is the local policy of client carriers (decisions.md, 2026-11-05, H11/H15; values
// are supplied by the network rules of T6.4):
//
//   - at most MaxPerPair concurrent carriers to one (address, SNI); further paths reuse the
//     existing session instead of opening a third carrier;
//   - after a penalty (handshake timeout or reset) an exponential backoff starts at BackoffBase
//     and doubles, capped at BackoffMax. Inside the window no new carrier is opened to the pair
//     and the uTLS profile is unchanged (the profile is not chosen here at all).
//
// A successful carrier resets the failure count.
type PathPolicy struct {
	max      int
	base     time.Duration
	maxDelay time.Duration
	clock    carrier.Clock

	mu    sync.Mutex
	pairs map[Pair]*pairState
}

type pairState struct {
	open     int
	failures int
	until    time.Time
}

// PathPolicyConfig configures a PathPolicy. Zero values select the policy defaults.
type PathPolicyConfig struct {
	// MaxPerPair is the carrier limit of one (address, SNI) pair (default 2).
	MaxPerPair int
	// BackoffBase is the first backoff after a penalty (default 120 s).
	BackoffBase time.Duration
	// BackoffMax caps the exponential growth (default 30 min).
	BackoffMax time.Duration
	// Clock is the injectable time source (AD13); nil means the system clock.
	Clock carrier.Clock
}

// PathLimitError: another carrier to this pair would exceed the limit; the caller must reuse
// the existing session (multiplexing) or pick another address or carrier.
type PathLimitError struct {
	Pair  Pair
	Limit int
}

func (e *PathLimitError) Error() string {
	return fmt.Sprintf("session: carrier pair %q (SNI %q) is at its limit of %d carriers",
		e.Pair.Host, e.Pair.SNI, e.Limit)
}

// PathBackoffError: the pair is inside a penalty backoff window.
type PathBackoffError struct {
	Pair  Pair
	Until time.Time
}

func (e *PathBackoffError) Error() string {
	return fmt.Sprintf("session: carrier pair %q (SNI %q) is backed off until %s",
		e.Pair.Host, e.Pair.SNI, e.Until.Format(time.RFC3339))
}

// NewPathPolicy returns a policy with every zero value replaced by its default.
func NewPathPolicy(cfg PathPolicyConfig) *PathPolicy {
	p := &PathPolicy{
		max:      cfg.MaxPerPair,
		base:     cfg.BackoffBase,
		maxDelay: cfg.BackoffMax,
		clock:    cfg.Clock,
		pairs:    map[Pair]*pairState{},
	}
	if p.max <= 0 {
		p.max = 2
	}
	if p.base <= 0 {
		p.base = 120 * time.Second
	}
	if p.maxDelay <= 0 {
		p.maxDelay = 30 * time.Minute
	}
	if p.clock == nil {
		p.clock = systemClock{}
	}
	return p
}

// Acquire reserves a carrier slot for the pair. It fails with *PathBackoffError inside a
// penalty window and with *PathLimitError when the pair already has MaxPerPair carriers.
func (p *PathPolicy) Acquire(pair Pair) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(pair)
	now := p.clock.Now()
	if st.until.After(now) {
		return &PathBackoffError{Pair: pair, Until: st.until}
	}
	if st.open >= p.max {
		return &PathLimitError{Pair: pair, Limit: p.max}
	}
	st.open++
	return nil
}

// Release returns a carrier slot without a penalty (a normal close or an abandoned path).
func (p *PathPolicy) Release(pair Pair) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(pair)
	if st.open > 0 {
		st.open--
	}
}

// Penalize returns the slot and starts (or extends) the exponential backoff of the pair. The
// penalty signal is a reset or a handshake timeout (H15).
func (p *PathPolicy) Penalize(pair Pair) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(pair)
	if st.open > 0 {
		st.open--
	}
	st.failures++
	d := p.base
	for i := 1; i < st.failures; i++ {
		if d >= p.maxDelay/2 {
			d = p.maxDelay
			break
		}
		d *= 2
	}
	if d > p.maxDelay {
		d = p.maxDelay
	}
	st.until = p.clock.Now().Add(d)
}

// Succeeded clears the failure count after a carrier to the pair worked.
func (p *PathPolicy) Succeeded(pair Pair) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(pair)
	st.failures = 0
	st.until = time.Time{}
}

// Backoff reports whether the pair is inside a penalty window and until when.
func (p *PathPolicy) Backoff(pair Pair) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(pair)
	return st.until, st.until.After(p.clock.Now())
}

// Open returns how many carriers of the pair the policy currently counts.
func (p *PathPolicy) Open(pair Pair) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state(pair).open
}

func (p *PathPolicy) state(pair Pair) *pairState {
	st := p.pairs[pair]
	if st == nil {
		st = &pairState{}
		p.pairs[pair] = st
	}
	return st
}
