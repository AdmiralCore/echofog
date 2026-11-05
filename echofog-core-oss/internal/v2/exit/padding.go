package exit

import (
	mrand "math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
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
	rnd     *mrand.Rand
	records int
	bytes   int
}

func newPadder(p *shape.Profile, sleep func(time.Duration), rnd *mrand.Rand) *padder {
	if sleep == nil {
		sleep = time.Sleep
	}
	return &padder{profile: p, sender: shape.NewSender(p, rnd, false), sleep: sleep, rnd: rnd}
}

// plan returns the PADDING payload length that rounds a DATA frame of n bytes up to the next
// shape bucket, and the jitter to wait first. Zero means no padding (no profile, the first 8
// records or 16 KiB are used up, or the budget says stop). The jitter is spent here, before the
// caller writes the record.
func (p *padder) plan(n int) (padPayload int, jitter time.Duration) {
	if p == nil || p.profile == nil || p.records >= padRecords || p.bytes >= padBytes {
		return 0, 0
	}
	record := frame.FrameLen(TypeData, n)
	pad := p.sender.PadFor(record)
	if pad < 3 {
		p.records++
		p.bytes += record
		return 0, 0
	}
	payload, ok := paddingPayload(pad)
	if !ok {
		p.records++
		p.bytes += record
		return 0, 0
	}
	p.records++
	p.bytes += record + pad
	jitter = 0
	span := int64(maxJitter) + 1
	if p.rnd != nil {
		jitter = time.Duration(p.rnd.Int64N(span))
	} else {
		jitter = time.Duration(mrand.Int64N(span))
	}
	if jitter > 0 {
		p.sleep(jitter)
	}
	return payload, jitter
}

// paddingPayload returns the payload length of a PADDING frame whose encoded size is exactly n,
// or false when no padding frame is that small. A PADDING frame is type(1) ‖ flags(1) ‖
// length(varint) ‖ payload, so its size grows with the payload's own varint width.
func paddingPayload(n int) (int, bool) {
	if n < 3 {
		return 0, false
	}
	// The payload is at most n-2; the varint width changes only at 64, 16384 and 2^30.
	for p := n - 2; p >= 0 && p >= n-10; p-- {
		if frame.FrameLen(frame.TypePadding, p) == n {
			return p, true
		}
	}
	return 0, false
}

// dataRecords is the number of DATA frames a write of n bytes becomes (services.md §7: DATA max
// 16384).
func dataRecords(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + frame.MaxMuxData - 1) / frame.MaxMuxData
}
