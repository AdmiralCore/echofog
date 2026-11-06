package call

import (
	"context"
	"errors"
	"sync"
	"time"

	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
)

// ErrJoinRefused is the only refusal a coordinator gives a join: the answer must not tell whether
// the call exists, the joiner was kicked, the room is full or the join was a replay (AD20).
var ErrJoinRefused = errors.New("call: join refused")

// JoinGuardConfig configures the coordinator's join admission.
type JoinGuardConfig struct {
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// Window is how far a join's timestamp may be from the local clock. Zero: 60 s.
	Window time.Duration
	// RefuseDelay is how long every refusal is held before it is sent. Zero: 250 ms.
	RefuseDelay time.Duration
	// MaxPending is how many admissions of one device may be in flight. Zero: 4.
	MaxPending int
	// MaxSalts bounds the remembered (device, join_salt) pairs. Zero: 4096.
	MaxSalts int
	// MaxSaltsPerDevice bounds the pairs one device may have remembered. Zero: 64.
	MaxSaltsPerDevice int
	// Allowed says whether the device may join this call now: the call exists, the device is not
	// kicked, the room has space. Nil allows all.
	Allowed func(callID string, device [32]byte) bool
}

// JoinGuard checks the freshness and novelty of joins on the coordinator (T9.1 finding 1).
//
// Every refusal goes through one function: the same error, the same delay, and the requester's
// admission slot is held until the answer leaves, also for the requests over MaxPending (AD20).
type JoinGuard struct {
	cfg JoinGuardConfig

	mu      sync.Mutex
	pending map[[32]byte]int
	salts   map[saltKey]time.Time
	order   []saltEntry // insertion order: the oldest first
}

type saltKey struct {
	device [32]byte
	salt   [JoinSaltLen]byte
}

type saltEntry struct {
	key saltKey
	at  time.Time
}

// NewJoinGuard returns a guard.
func NewJoinGuard(cfg JoinGuardConfig) *JoinGuard {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Window == 0 {
		cfg.Window = 60 * time.Second
	}
	if cfg.RefuseDelay == 0 {
		cfg.RefuseDelay = 250 * time.Millisecond
	}
	if cfg.MaxPending == 0 {
		cfg.MaxPending = 4
	}
	if cfg.MaxSalts == 0 {
		cfg.MaxSalts = 4096
	}
	return &JoinGuard{cfg: cfg, pending: map[[32]byte]int{}, salts: map[saltKey]time.Time{}}
}

// Admit accepts a verified join or refuses it with ErrJoinRefused. timestampMs is the signal's
// timestamp. A join is refused when its timestamp is outside the window, when the (device,
// join_salt) pair was admitted before, when the join is malformed or when Allowed says no.
func (g *JoinGuard) Admit(ctx context.Context, callID string, device [32]byte, j *pb.Join, timestampMs int64) error {
	g.mu.Lock()
	over := g.pending[device] >= g.cfg.MaxPending
	g.pending[device]++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		if g.pending[device]--; g.pending[device] <= 0 {
			delete(g.pending, device)
		}
		g.mu.Unlock()
	}()

	if over || !g.admit(callID, device, j, timestampMs) {
		select {
		case <-g.cfg.After(g.cfg.RefuseDelay):
		case <-ctx.Done():
		}
		return ErrJoinRefused
	}
	return nil
}

func (g *JoinGuard) admit(callID string, device [32]byte, j *pb.Join, timestampMs int64) bool {
	if ValidateJoin(j) != nil {
		return false
	}
	now := g.cfg.Now()
	skew := now.Sub(time.UnixMilli(timestampMs))
	if skew > g.cfg.Window || skew < -g.cfg.Window {
		return false
	}
	if g.cfg.Allowed != nil && !g.cfg.Allowed(callID, device) {
		return false
	}
	var k saltKey
	k.device = device
	copy(k.salt[:], j.JoinSalt)

	g.mu.Lock()
	defer g.mu.Unlock()
	// A pair is remembered for twice the window: after that its timestamp is stale anyway.
	retain := 2 * g.cfg.Window
	for len(g.order) > 0 && (now.Sub(g.order[0].at) > retain || len(g.order) >= g.cfg.MaxSalts) {
		e := g.order[0]
		g.order = g.order[1:]
		if g.salts[e.key] == e.at {
			delete(g.salts, e.key)
		}
	}
	if at, ok := g.salts[k]; ok && now.Sub(at) <= retain {
		return false
	}
	g.salts[k] = now
	g.order = append(g.order, saltEntry{key: k, at: now})
	return true
}
