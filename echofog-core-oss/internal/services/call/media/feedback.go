package media

import (
	"sync"
	"time"
)

// What a viewer tells the broadcaster about its path. The broadcaster's own rate control only sees
// its send queue; a link that drops or delays packets further on (a laptop's Wi-Fi, a provider)
// went unnoticed, the broadcaster kept sending what the link could not carry, and every burst of
// loss cost the viewer a keyframe, which made the next burst worse. So each viewer measures, every
// FeedbackInterval, the share of a stream's units that did not come the first time (it had to ask
// for them again), how far the fragments queue up on the way, and how much arrives; the call layer
// sends the worst of its subtree up the stream's tree.
//
// The delay needs no clock in common: every fragment carries the time its unit was sealed at the
// broadcaster, so arrival minus that time is the path's delay plus a constant offset between the
// clocks. Its lowest value over the last seconds stands for the empty path; how far the fragments
// of a moment are above it is the queue.

// FeedbackInterval is how often a viewer measures and reports.
const FeedbackInterval = 250 * time.Millisecond

const (
	feedbackBaseline = 40 // windows the lowest delay is kept over: 10 s
	feedbackMinUnits = 30 // fewer units in two windows say nothing about loss
	// feedbackLateMs: a fragment sealed this long before the newest one is a resend (it comes a
	// NACK's wait and a round trip later), not a first arrival
	feedbackLateMs = 25
	// feedbackJump: a stream that moves this far at once restarted, or we joined it late
	feedbackJump = 4096
)

// Congestion is how loaded a viewer's path is, from free to overflowing; the broadcaster's rate
// control treats it like the state of its own send queue.
type Congestion uint8

const (
	PathClean Congestion = iota // the rate may climb
	PathHold                    // some loss or queueing: the rate stays
	PathMild                    // the path is filling up: the rate goes down a little
	PathHeavy                   // the path cannot carry the rate
	PathFlood                   // much of the stream is lost
)

// Feedback is one window of a viewer's measurement of a stream.
type Feedback struct {
	Level   Congestion
	Kbps    int     // what arrived the first time, the rate the path carried
	Loss    float64 // the share of units that did not come the first time
	DelayMs int     // the queue: how far the window's delay is above the path's lowest
}

// congestionOf grades a window by the loss of the last two (one window of a quarter second is
// a few hundred units, too few to tell 10% from 15%) and a burst in the last one alone. Delay
// must last two windows, as one large frame (a keyframe) fills the path for a moment on its own.
func congestionOf(loss, burst float64, lossKnown bool, q, prevQ int) Congestion {
	both := func(ms int) bool { return q >= ms && prevQ >= ms }
	switch {
	case lossKnown && (loss >= 0.25 || burst >= 0.30), both(500):
		return PathFlood
	case lossKnown && loss >= 0.10, both(200):
		return PathHeavy
	case lossKnown && loss >= 0.05, q >= 80 && prevQ >= 40 && q > prevQ, both(120):
		return PathMild
	case lossKnown && loss >= 0.02, q >= 40:
		return PathHold
	}
	return PathClean
}

// rxMeter measures one watched stream. Loss is what this node had to ask its parent for: a unit
// still missing when the reorder wait of the first NACK ran out (nackGap), against the units the
// stream moved on by; packets that only came out of order are not lost. A resend carries the time
// its unit was first sealed, far behind the newest: it is left out of the delay and the rate.
type rxMeter struct {
	started   bool
	highest   uint32  // the newest unit seen
	newestTs  uint32  // the newest sealing time seen
	units     float64 // units the stream moved on by in this window
	lost      float64 // ... that had to be asked for
	prevUnits float64 // the same for the window before
	prevLost  float64
	bytes     int
	delaySum  int64
	delayN    int
	delayMin  int64
	baselines [feedbackBaseline]int64 // the lowest delay of each recent window
	nBase     int
	prevQ     int
}

func newRxMeter() *rxMeter { return &rxMeter{} }

// add takes a fragment that arrived at now (ms on this node's clock).
func (r *rxMeter) add(seq uint32, ts uint32, size int, now int64) {
	if jump := int32(seq - r.highest); !r.started || jump > feedbackJump || jump < -feedbackJump {
		// the first fragment, or the stream jumped: count from here on, keep the path's delay
		r.started, r.highest, r.newestTs = true, seq, ts
		r.units, r.lost, r.prevUnits, r.prevLost = 0, 0, 0, 0
	}
	if jump := int32(seq - r.highest); jump > 0 {
		r.units += float64(jump)
		r.highest = seq
	}
	if int32(ts-r.newestTs) > 0 {
		r.newestTs = ts
	}
	if int32(r.newestTs-ts) > feedbackLateMs {
		return // a resend
	}
	r.bytes += size
	d := int64(int32(uint32(now) - ts)) // arrival minus sealing, both in ms; wraps every 49 days
	if r.delayN == 0 || d < r.delayMin {
		r.delayMin = d
	}
	r.delaySum += d
	r.delayN++
}

// window closes the measurement of the last interval.
func (r *rxMeter) window(interval time.Duration) Feedback {
	var fb Feedback
	if !r.started {
		return fb
	}
	fb.Kbps = int(int64(r.bytes) * 8 / max(interval.Milliseconds(), 1))
	units, lost := r.units+r.prevUnits, r.lost+r.prevLost
	lossKnown := units >= feedbackMinUnits
	burst := 0.0
	if units > 0 {
		fb.Loss = min(1, lost/units)
	}
	if r.units >= feedbackMinUnits {
		burst = min(1, r.lost/r.units)
	}
	if r.delayN > 0 {
		r.baselines[r.nBase%feedbackBaseline] = r.delayMin
		r.nBase++
		base := r.baselines[0]
		for i := 1; i < min(r.nBase, feedbackBaseline); i++ {
			base = min(base, r.baselines[i])
		}
		fb.DelayMs = int(max(0, r.delaySum/int64(r.delayN)-base))
	}
	fb.Level = congestionOf(fb.Loss, burst, lossKnown, fb.DelayMs, r.prevQ)
	r.prevQ = fb.DelayMs
	if r.units >= feedbackMinUnits/2 {
		r.prevUnits, r.prevLost = r.units, r.lost
		r.units, r.lost = 0, 0
	}
	r.bytes, r.delaySum, r.delayN = 0, 0, 0
	return fb
}

// rxMeters keeps a meter per watched stream.
type rxMeters struct {
	mu     sync.Mutex
	meters map[string]*rxMeter
}

func (m *rxMeters) add(p *MediaPacket, size int, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.meters == nil {
		m.meters = make(map[string]*rxMeter)
	}
	r := m.meters[p.StreamID]
	if r == nil {
		r = newRxMeter()
		m.meters[p.StreamID] = r
	}
	r.add(p.Seq, p.Timestamp, size, now.UnixMilli())
}

// lost counts units of a stream this node asks its parent for the first time.
func (m *rxMeters) lost(streamID string, units int) {
	m.mu.Lock()
	if r := m.meters[streamID]; r != nil {
		r.lost += float64(units)
	}
	m.mu.Unlock()
}

func (m *rxMeters) drop(streamID string) {
	m.mu.Lock()
	delete(m.meters, streamID)
	m.mu.Unlock()
}

// StreamFeedback closes the window of every watched stream that sent something since it started
// and returns what the path did; the call layer calls it every FeedbackInterval.
func (d *MediaDispatcher) StreamFeedback(interval time.Duration) map[string]Feedback {
	d.rx.mu.Lock()
	defer d.rx.mu.Unlock()
	out := make(map[string]Feedback, len(d.rx.meters))
	for id, r := range d.rx.meters {
		if r.started {
			out[id] = r.window(interval)
		}
	}
	return out
}
