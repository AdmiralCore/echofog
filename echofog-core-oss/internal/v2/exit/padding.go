package exit

import (
	"math/rand/v2"
	"time"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// H1: pad the first records of each new exit stream direction.
const (
	padRecords = 8
	padBytes   = 16 << 10
	maxJitter  = 30 * time.Millisecond
)

// padder draws the PADDING frames of one stream direction (H1). It is not safe for concurrent
// use; each direction owns one.
type padder struct {
	profile *shape.Profile
	sender  *shape.Sender
	sleep   func(time.Duration)
	rnd     *rand.Rand
	records int
	bytes   int
}

func newPadder(p *shape.Profile, sleep func(time.Duration), rnd *rand.Rand) *padder {
	if sleep == nil {
		sleep = time.Sleep
	}
	return &padder{profile: p, sender: shape.NewSender(p, rnd, false), sleep: sleep, rnd: rnd}
}

// plan returns the PADDING payload length that rounds a DATA frame of n bytes up to the next
// shape bucket, and the jitter to wait first. Zero means no padding (no profile, the first 8
// records or 16 KiB are used up, or the budget says stop).
func (p *padder) plan(n int) (padPayload int, jitter time.Duration) { return 0, 0 }

// paddingPayload returns the payload length of a PADDING frame whose encoded size is exactly n,
// or false when no padding frame is that small.
func paddingPayload(n int) (int, bool) { return 0, false }

// dataRecords splits b into DATA frames of at most frame.MaxMuxData.
func dataRecords(b []byte) int { return 0 }
