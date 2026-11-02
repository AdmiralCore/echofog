package media

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// feed sends a window's worth of units (one fragment each, 1200 bytes) from seq on: each one
// sealed in turn and arriving delay(i) ms later on a clock 5 s off, unless lost(i), which the
// dispatcher's NACK counts.
func feed(r *rxMeter, seq *uint32, n int, start int64, lost func(i int) bool, delay func(i int) int64) {
	for i := 0; i < n; i++ {
		*seq++
		sealed := start + int64(i)*int64(FeedbackInterval/time.Millisecond)/int64(n)
		if lost != nil && lost(i) {
			r.lost++
			continue
		}
		d := int64(20)
		if delay != nil {
			d = delay(i)
		}
		r.add(*seq, uint32(sealed), 1200, sealed+d+5000)
	}
}

func TestFeedback_ACleanPathIsClean(t *testing.T) {
	r := newRxMeter()
	var seq uint32
	var fb Feedback
	for w := 0; w < 8; w++ {
		feed(r, &seq, 250, int64(w)*250, nil, func(i int) int64 { return 20 + int64(i%7) }) // jitter of a few ms
		fb = r.window(FeedbackInterval)
	}
	assert.Equal(t, PathClean, fb.Level)
	assert.InDelta(t, 0, fb.Loss, 0.001)
	assert.Less(t, fb.DelayMs, 10)
	assert.InDelta(t, 9600, fb.Kbps, 100, "250 units of 1200 bytes in 250 ms")
}

func TestFeedback_LossIsWhatHadToBeAskedFor(t *testing.T) {
	r := newRxMeter()
	var seq uint32
	rng := rand.New(rand.NewPCG(1, 2))
	var fb Feedback
	for w := 0; w < 8; w++ {
		first := seq
		feed(r, &seq, 250, int64(w)*250, func(int) bool { return rng.Float64() < 0.15 }, nil)
		// every unit comes again later, as the parent resends what the NACKs asked for
		for s := first + 1; s != seq-20; s++ {
			r.add(s, uint32(w*250), 1200, int64(w*250+5300))
		}
		fb = r.window(FeedbackInterval)
	}
	assert.InDelta(t, 0.15, fb.Loss, 0.05)
	assert.Equal(t, PathHeavy, fb.Level)

	// a burst that takes a third of the stream
	feed(r, &seq, 250, 8*250, func(i int) bool { return i%3 == 0 }, nil)
	fb = r.window(FeedbackInterval)
	assert.Equal(t, PathFlood, fb.Level)
}

func TestFeedback_AFillingQueueIsSeenBeforeLoss(t *testing.T) {
	r := newRxMeter()
	var seq uint32
	for w := 0; w < 4; w++ {
		feed(r, &seq, 250, int64(w)*250, nil, nil)
		r.window(FeedbackInterval)
	}
	// the link carries less than is sent: each window waits 60 ms longer
	var levels []Congestion
	for w := 4; w < 10; w++ {
		q := int64(w-3) * 60
		feed(r, &seq, 250, int64(w)*250, nil, func(int) int64 { return 20 + q })
		levels = append(levels, r.window(FeedbackInterval).Level)
	}
	assert.Equal(t, []Congestion{PathHold, PathMild, PathMild, PathMild, PathHeavy, PathHeavy}, levels)
}

func TestFeedback_OneLargeFrameIsNotCongestion(t *testing.T) {
	r := newRxMeter()
	var seq uint32
	for w := 0; w < 4; w++ {
		feed(r, &seq, 250, int64(w)*250, nil, nil)
		r.window(FeedbackInterval)
	}
	// a keyframe holds the path for 150 ms, the next window is back to normal
	feed(r, &seq, 250, 4*250, nil, func(i int) int64 { return 20 + 150 - int64(i)*150/250 })
	assert.LessOrEqual(t, r.window(FeedbackInterval).Level, PathHold)
	feed(r, &seq, 250, 5*250, nil, nil)
	assert.Equal(t, PathClean, r.window(FeedbackInterval).Level)
}

func TestFeedback_AJumpStartsOverInsteadOfCountingLoss(t *testing.T) {
	r := newRxMeter()
	seq := uint32(100)
	feed(r, &seq, 250, 0, nil, nil)
	r.window(FeedbackInterval)
	seq += 20000 // the broadcaster restarted its stream
	feed(r, &seq, 250, 250, nil, nil)
	fb := r.window(FeedbackInterval)
	assert.Equal(t, PathClean, fb.Level)
	assert.InDelta(t, 0, fb.Loss, 0.001)
}
