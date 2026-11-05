package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
)

// Session table policy. The limits are local (platforms.md §2): the wire carries no session
// count, a refusal is an ordinary GOAWAY.
const (
	// MobileMaxSessions is the platform limit for a device with mem_cap_mb set (platforms.md
	// §2: at most 32 sessions).
	MobileMaxSessions = 32
	// DesktopMaxSessions is this build's local limit for a desktop or server profile. It is
	// not in the specification; it only bounds memory on a long-running node.
	DesktopMaxSessions = 256

	mobileIdle  = 2 * time.Minute
	desktopIdle = 10 * time.Minute

	mobileSweep  = 15 * time.Second
	desktopSweep = 60 * time.Second
)

// MaxSessionsFor returns the session limit of a platform profile: 32 on mobile (mem_cap_mb is
// positive, platforms.md §2) and a larger local bound elsewhere.
func MaxSessionsFor(memCapMB int) int {
	if memCapMB > 0 {
		return MobileMaxSessions
	}
	return DesktopMaxSessions
}

func idleFor(memCapMB int) time.Duration {
	if memCapMB > 0 {
		return mobileIdle
	}
	return desktopIdle
}

func sweepFor(memCapMB int) time.Duration {
	if memCapMB > 0 {
		return mobileSweep
	}
	return desktopSweep
}

// Key is the session table key of AD5: the local identity's device ID and the peer's device
// ID. A device ID is unique per identity (a throwaway or exit identity has its own), so the
// key separates the main identity, one-use identities and per-exit identities.
type Key struct {
	Local [32]byte
	Peer  [32]byte
}

// OpenFunc establishes a new session for a key. The table appends its own Dial options (the
// CARD_UPDATE handler), so the callback must pass opts on to Dial.
type OpenFunc func(ctx context.Context, opts ...DialOption) (*Established, error)

// TableConfig configures a session table. Zero values select the profile defaults.
type TableConfig struct {
	// MemCapMB is the platform flag of the same name (platforms.md §2). A positive value
	// selects the mobile limits.
	MemCapMB int
	// MaxSessions overrides the limit derived from MemCapMB (tests, server policy).
	MaxSessions int
	// IdleTimeout closes a session with no open streams and no traffic for this long.
	IdleTimeout time.Duration
	// SweepInterval is how often idle sessions and the limit are checked; every round is
	// jittered by ±20 % (AD13).
	SweepInterval time.Duration
	// RetryAfter is the GOAWAY retry_after of duplicate, idle and over-limit closes; seconds.
	RetryAfter uint32
	// Clock is the injectable time source (AD13). Activity times themselves come from the mux.
	Clock carrier.Clock
	// Control handles CARD_UPDATE for sessions opened through the table. Pass the same value
	// to AcceptConfig.Control on the accepting side.
	Control *Control
}

type flight struct {
	done chan struct{}
	est  *Established
	err  error
}

// Table keeps one live session per (local identity, peer device) and coalesces concurrent
// dials to the same key (AD5, wiring.md T3.2).
type Table struct {
	cfg   TableConfig
	clock carrier.Clock
	max   int
	idle  time.Duration
	sweep time.Duration

	mu       sync.Mutex
	sessions map[Key]*Established
	flights  map[Key]*flight
	closed   bool
	timer    carrier.Timer
}

// NewTable returns a session table and starts its idle/limit sweeper.
func NewTable(cfg TableConfig) (*Table, error) {
	t := &Table{
		cfg:      cfg,
		clock:    clockOr(cfg.Clock),
		max:      cfg.MaxSessions,
		idle:     cfg.IdleTimeout,
		sweep:    cfg.SweepInterval,
		sessions: map[Key]*Established{},
		flights:  map[Key]*flight{},
	}
	if t.max <= 0 {
		t.max = MaxSessionsFor(cfg.MemCapMB)
	}
	if t.idle <= 0 {
		t.idle = idleFor(cfg.MemCapMB)
	}
	if t.sweep <= 0 {
		t.sweep = sweepFor(cfg.MemCapMB)
	}
	t.schedule()
	return t, nil
}

// Control returns the CARD_UPDATE handler of the table (nil when TableConfig.Control is nil).
// The accepting side passes it in AcceptConfig.Control.
func (t *Table) Control() *Control { return t.cfg.Control }

// Len returns the number of sessions currently kept.
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sessions)
}

// Max returns the session limit of the table (from mem_cap_mb or TableConfig.MaxSessions).
func (t *Table) Max() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.max
}

// Lookup returns the live session for (local, peer), or nil.
func (t *Table) Lookup(local, peer [32]byte) *Established {
	t.mu.Lock()
	defer t.mu.Unlock()
	est := t.sessions[Key{Local: local, Peer: peer}]
	if est == nil || !sessionUsable(est) {
		return nil
	}
	return est
}

// ByPeer returns a live session to the peer device, of any local identity. It is used by path
// operations that are local policy and do not carry an identity.
func (t *Table) ByPeer(peer [32]byte) *Established {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, est := range t.sessions {
		if est.Peer != nil && est.Peer.DeviceID == peer && sessionUsable(est) {
			return est
		}
	}
	return nil
}

// Sessions returns a snapshot of the sessions currently kept. A draining session stays here until
// its successor replaces it, exactly as for Get; callers that act on the snapshot check the
// session state themselves.
func (t *Table) Sessions() []*Established {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*Established, 0, len(t.sessions))
	for _, est := range t.sessions {
		out = append(out, est)
	}
	return out
}

func (t *Table) schedule() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.timer = t.clock.AfterFunc(jitter(t.sweep), func() {
		t.sweepOnce()
		t.schedule()
	})
	t.mu.Unlock()
}

// jitter returns d in [0.8d, 1.2d] (AD13).
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*mrand.Float64()))
}

// sweepOnce closes sessions idle beyond the profile timeout and evicts the least recently
// active ones above the limit. GOAWAY drains each victim in its own goroutine so the sweep
// never waits 15 s.
func (t *Table) sweepOnce() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	now := time.Now()
	var victims []*Established
	for key, est := range t.sessions {
		if sessionUsable(est) {
			continue
		}
		delete(t.sessions, key)
	}
	for key, est := range t.sessions {
		if idleSession(est, now, t.idle) {
			delete(t.sessions, key)
			victims = append(victims, est)
		}
	}
	if len(t.sessions) > t.max {
		victims = append(victims, t.evictLocked(nil)...)
	}
	t.mu.Unlock()

	for _, est := range victims {
		go func(est *Established) {
			_ = est.GoAway(context.Background(), frame.CodeGoingAway, "idle", t.cfg.RetryAfter)
		}(est)
	}
}

// evictLocked removes the least recently active sessions above the limit and returns them.
// keep, when not nil, is never evicted (the session the caller just created).
func (t *Table) evictLocked(keep *Established) []*Established {
	if len(t.sessions) <= t.max {
		return nil
	}
	live := make([]*Established, 0, len(t.sessions))
	for _, est := range t.sessions {
		if est != keep {
			live = append(live, est)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		return lastActivity(live[i]).Before(lastActivity(live[j]))
	})
	over := len(t.sessions) - t.max
	if over > len(live) {
		over = len(live)
	}
	victims := make([]*Established, 0, over)
	for _, est := range live[:over] {
		for key, e := range t.sessions {
			if e == est {
				delete(t.sessions, key)
				break
			}
		}
		victims = append(victims, est)
	}
	return victims
}

// Get returns the live session for (local, peer). Concurrent callers for the same key share one
// dial; the first to finish stores the session and every waiter receives it. A session that is
// closed or draining is replaced.
func (t *Table) Get(ctx context.Context, local LocalIdentity, peer [32]byte, open OpenFunc) (*Established, error) {
	if open == nil {
		return nil, fmt.Errorf("%w: no open function", ErrConfig)
	}
	lid, err := localDeviceID(local)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := Key{Local: lid, Peer: peer}

	for {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return nil, fmt.Errorf("%w: table is closed", ErrConfig)
		}
		if est := t.sessions[key]; est != nil {
			if sessionUsable(est) {
				t.mu.Unlock()
				return est, nil
			}
			delete(t.sessions, key)
		}
		if f := t.flights[key]; f != nil {
			t.mu.Unlock()
			select {
			case <-f.done:
				if f.err != nil {
					return nil, f.err
				}
				if sessionUsable(f.est) {
					return f.est, nil
				}
				// A simultaneous accept replaced the dialed session; take the survivor.
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		f := &flight{done: make(chan struct{})}
		t.flights[key] = f
		t.mu.Unlock()

		var opts []DialOption
		if t.cfg.Control != nil {
			opts = append(opts, WithControl(t.cfg.Control))
		}
		est, err := open(ctx, opts...)

		var kept, loser *Established
		if err == nil {
			switch {
			case est == nil || est.Session == nil:
				err = fmt.Errorf("%w: dial returned no session", ErrConfig)
			case est.Peer == nil:
				err = fmt.Errorf("%w: dial returned no peer card", ErrConfig)
			case est.Peer.DeviceID != peer:
				err = fmt.Errorf("%w: dialed device %x, table key expected %x", ErrConfig, est.Peer.DeviceID, peer)
				_ = est.Session.Close()
			default:
				t.mu.Lock()
				kept, loser = t.insertLocked(key, est)
				evicted := t.evictLocked(kept)
				t.mu.Unlock()
				for _, old := range evicted {
					go func(old *Established) {
						_ = old.GoAway(context.Background(), frame.CodeGoingAway, "over limit", t.cfg.RetryAfter)
					}(old)
				}
			}
		}

		t.mu.Lock()
		delete(t.flights, key)
		if err != nil {
			f.err = err
		} else {
			f.est = kept
		}
		close(f.done)
		t.mu.Unlock()

		if loser != nil {
			go t.closeDuplicate(loser)
		}
		if err != nil {
			return nil, err
		}
		if !sessionUsable(kept) {
			// Our session lost the duplicate race after it was stored; return the survivor.
			continue
		}
		return kept, nil
	}
}

// Add registers an accepted session. When a live session already exists for the key, the
// duplicate rule of decisions.md T3.2 keeps the session whose initiator has the smaller device
// ID; the other one is closed with GOAWAY{no-error}. Add returns the survivor.
func (t *Table) Add(local LocalIdentity, est *Established) (*Established, error) {
	lid, err := localDeviceID(local)
	if err != nil {
		return nil, err
	}
	if est == nil || est.Session == nil || est.Peer == nil {
		return nil, fmt.Errorf("%w: no session to add", ErrConfig)
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: table is closed", ErrConfig)
	}
	kept, loser := t.insertLocked(Key{Local: lid, Peer: est.Peer.DeviceID}, est)
	evicted := t.evictLocked(kept)
	t.mu.Unlock()
	if loser != nil {
		go t.closeDuplicate(loser)
	}
	for _, old := range evicted {
		go func(old *Established) {
			_ = old.GoAway(context.Background(), frame.CodeGoingAway, "over limit", t.cfg.RetryAfter)
		}(old)
	}
	return kept, nil
}

// insertLocked stores est unless a live session already exists. The survivor is the session
// whose initiator has the smaller device ID, compared bytewise (decisions.md T3.2). Equal
// initiators (only a self-dial) keep the stored session.
func (t *Table) insertLocked(key Key, est *Established) (kept, loser *Established) {
	old := t.sessions[key]
	if old == nil || !sessionUsable(old) {
		t.sessions[key] = est
		return est, nil
	}
	if old == est {
		return est, nil
	}
	if keepSession(old, est) == old {
		return old, est
	}
	t.sessions[key] = est
	return est, old
}

// keepSession returns the session to keep: the one whose initiator has the smaller device ID.
func keepSession(a, b *Established) *Established {
	ia, oka := initiatorDeviceID(a)
	ib, okb := initiatorDeviceID(b)
	if !oka {
		return b
	}
	if !okb {
		return a
	}
	if bytes.Compare(ia[:], ib[:]) <= 0 {
		return a
	}
	return b
}

func initiatorDeviceID(e *Established) ([32]byte, bool) {
	if e == nil {
		return [32]byte{}, false
	}
	if e.Role == RoleInitiator {
		if e.Local.Device == nil {
			return [32]byte{}, false
		}
		return e.Local.Device.ID(), true
	}
	if e.Peer == nil {
		return [32]byte{}, false
	}
	return e.Peer.DeviceID, true
}

func (t *Table) closeDuplicate(est *Established) {
	_ = est.GoAway(context.Background(), frame.CodeNoError, "", t.cfg.RetryAfter)
}

// PushCard sends a new card of the local identity to every live session that negotiated
// card-push. It returns the number of peers it reached.
func (t *Table) PushCard(local [32]byte, envelope []byte) (int, error) {
	t.mu.Lock()
	var peers []*Established
	for key, est := range t.sessions {
		if key.Local == local && sessionUsable(est) {
			peers = append(peers, est)
		}
	}
	t.mu.Unlock()

	sent := 0
	var errs []error
	for _, est := range peers {
		if !est.Session.HasFeature(frame.FeatureCardPush) {
			continue
		}
		if err := est.Session.SendControl(frame.TypeCardUpdate, 0, envelope); err != nil {
			errs = append(errs, err)
			continue
		}
		sent++
	}
	return sent, errors.Join(errs...)
}

// Close stops the sweeper and announces a normal shutdown to every session. The sessions drain
// in the background, as wire.md §7.3 requires.
func (t *Table) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	if t.timer != nil {
		t.timer.Stop()
	}
	sessions := make([]*Established, 0, len(t.sessions))
	for _, est := range t.sessions {
		sessions = append(sessions, est)
	}
	t.sessions = map[Key]*Established{}
	t.mu.Unlock()

	for _, est := range sessions {
		go func(est *Established) {
			_ = est.GoAway(context.Background(), frame.CodeGoingAway, "shutdown", t.cfg.RetryAfter)
		}(est)
	}
	return nil
}

func localDeviceID(local LocalIdentity) ([32]byte, error) {
	if local.Device == nil {
		return [32]byte{}, fmt.Errorf("%w: no local device", ErrConfig)
	}
	return local.Device.ID(), nil
}

func sessionUsable(est *Established) bool {
	if est == nil || est.Session == nil {
		return false
	}
	select {
	case <-est.Session.Done():
		return false
	default:
	}
	return !est.Session.GoingAway()
}

func idleSession(est *Established, now time.Time, idle time.Duration) bool {
	if est == nil || est.Session == nil {
		return true
	}
	local, peer := est.Session.StreamCounts()
	if local+peer > 0 {
		return false
	}
	return now.Sub(lastActivity(est)) >= idle
}

func lastActivity(est *Established) time.Time {
	if est == nil || est.Session == nil {
		return time.Time{}
	}
	t := est.Session.LastActivity()
	if t.IsZero() {
		return time.Now()
	}
	return t
}
