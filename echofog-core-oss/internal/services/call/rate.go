package call

import (
	"time"

	"github.com/echofog/echofog-core/internal/services/call/media"
)

// The browser encoder only learns about the loopback to the bridge, which never congests. This
// loop watches the real path (outbox drops and queue depth) and caps the encoder with REMB: a
// little when the path starts to back up, more when it is full, then it waits for the cut to show
// before cutting again, and climbs back only after the path has stayed clean for a while, never
// above the preset. A jumpy rate makes a jumpy picture: each cut costs the encoder frames or
// detail, so a single late frame is not worth one.
const (
	rateInterval  = 500 * time.Millisecond
	rateCutMild   = 0.9  // a few drops, or a queue filling up
	rateCutHeavy  = 0.75 // many drops, or a queue half full
	rateClimb     = 1.06
	rateCooldown  = 2 // ticks after a cut before the next one
	rateCleanRun  = 4 // clean ticks before climbing back
	rateFloorKbps = 600
	heavyDrops    = 8 // new drops in one tick that make it heavy
)

type sendRateState struct {
	kbps        float64
	lastDropped uint64
	at          time.Time
	sinceCut    int // ticks since the last cut
	clean       int // clean ticks in a row
}

// congestion is how backed up the path is in one tick.
type congestion int

const (
	pathClean congestion = iota
	pathMild
	pathHeavy
)

func (m *CallManager) rateTickLocked(room *ActiveRoomState, now time.Time) {
	r := &room.sendRate
	if now.Sub(r.at) < rateInterval {
		return
	}
	r.at = now
	br := room.Bridge
	if br == nil || room.Dispatcher == nil || len(room.localStreams) == 0 {
		r.kbps = 0
		return
	}
	capKbps := 0
	for _, ann := range room.localStreams {
		capKbps += ann.BitrateKbps
	}
	if capKbps <= 0 {
		return
	}

	dropped, backlog := room.Dispatcher.VideoPressure()
	newDrops := dropped - r.lastDropped
	r.lastDropped = dropped
	level := pathClean
	switch {
	case newDrops >= heavyDrops || backlog > media.VideoQueueLen/2:
		level = pathHeavy
	case newDrops > 0 || backlog > media.VideoQueueLen/4:
		level = pathMild
	}
	r.kbps = r.next(float64(capKbps), float64(rateFloorKbps*len(room.localStreams)), level)
	if level != pathClean {
		m.debugOnce("congested", "Upload path congested, lowering the send bitrate",
			"kbps", int(r.kbps), "cap_kbps", capKbps, "new_drops", newDrops, "backlog", backlog)
	}

	type limit struct {
		role string
		bps  uint64
	}
	var limits []limit
	for _, ann := range room.localStreams {
		share := r.kbps * float64(ann.BitrateKbps) / float64(capKbps)
		limits = append(limits, limit{captureRole(ann.MediaType), uint64(share * 1000)})
	}
	m.later(func() {
		for _, l := range limits {
			_ = br.SetSendBitrate(l.role, l.bps) // the bridge logs failures, rate limited
		}
	})
}

// next is one step of the rate; zero means no rate yet, which starts at the preset.
func (r *sendRateState) next(capKbps, floor float64, level congestion) float64 {
	kbps := r.kbps
	r.sinceCut++
	if level == pathClean {
		r.clean++
	} else {
		r.clean = 0
	}
	switch {
	case kbps == 0:
		kbps = capKbps
	case level != pathClean && r.sinceCut > rateCooldown:
		cut := rateCutMild
		if level == pathHeavy {
			cut = rateCutHeavy
		}
		kbps *= cut
		r.sinceCut = 0
	case level == pathClean && r.clean >= rateCleanRun:
		kbps *= rateClimb
	}
	return min(max(kbps, floor), capKbps)
}
