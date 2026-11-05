package session

import (
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// FeaturesFor returns the feature list a platform profile offers. A low_power platform does not
// offer idle-padding (platforms.md §2): standby traffic costs battery, so the feature is never
// negotiated there.
func FeaturesFor(base []uint32, lowPower bool) []uint32 {
	out := make([]uint32, 0, len(base))
	for _, f := range base {
		if lowPower && f == uint32(frame.FeatureIdlePadding) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// idlePadding adapts a shape.Sender to the mux idle-padding hook. A Sender owns one direction
// and is not safe for concurrent use, so the adapter serialises it.
type idlePadding struct {
	mu sync.Mutex
	s  *shape.Sender
}

// newIdlePadding builds the sender of one session's padding-only packets. A nil profile and
// low_power both disable idle padding.
func newIdlePadding(p *shape.Profile, r *mrand.Rand, lowPower bool) *idlePadding {
	if p == nil || lowPower {
		return nil
	}
	return &idlePadding{s: shape.NewSender(p, r, lowPower)}
}

// next draws the gap until the next padding-only frame and its payload size (wire.md §6.4, §9).
func (i *idlePadding) next() (time.Duration, int, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	gap, ok := i.s.IdlePad()
	if !ok {
		return 0, 0, false
	}
	return gap, i.s.PadFor(0), true
}
