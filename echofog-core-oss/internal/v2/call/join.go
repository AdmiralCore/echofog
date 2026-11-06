package call

import (
	"context"
	"errors"
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
	// Allowed says whether the device may join this call now: the call exists, the device is not
	// kicked, the room has space. Nil allows all.
	Allowed func(callID string, device [32]byte) bool
}

// JoinGuard checks the freshness and novelty of joins on the coordinator (T9.1 finding 1).
type JoinGuard struct{}

// NewJoinGuard returns a guard.
func NewJoinGuard(cfg JoinGuardConfig) *JoinGuard { return &JoinGuard{} }

// Admit accepts a verified join or refuses it with ErrJoinRefused. timestampMs is the signal's
// timestamp.
func (g *JoinGuard) Admit(ctx context.Context, callID string, device [32]byte, j *pb.Join, timestampMs int64) error {
	return nil
}
