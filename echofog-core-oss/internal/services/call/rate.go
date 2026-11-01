package call

import (
	"time"

	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
)

// The browser encoder only learns about the loopback to the bridge, which never congests. This
// loop watches the real path (outbox drops and queue depth) and caps the encoder with REMB (an
// encoder outside the webview, through SetLocalEncoder): a little when the path starts to back
// up, more when it is full, by half when the queue overflowed, then it waits for the cut to show
// before cutting again, and climbs back only after the path has stayed clean for a while, never
// above the preset. A jumpy rate makes a jumpy picture: each cut costs the encoder frames or
// detail, so a single late frame is not worth one.
//
// It remembers the rate a trouble began at and climbs back to it slowly: climbing at full speed
// took a 120 fps stream from 7 back to 20 Mbit/s in ten seconds, straight into the next overflow
// of a link that carried 15, and every overflow cost the viewer hundreds of packets and a
// keyframe. The rate is forgotten after a while clean, as links get better too.
const (
	rateInterval   = 500 * time.Millisecond
	rateCutMild    = 0.9  // a few drops, or a queue filling up
	rateCutHeavy   = 0.75 // many drops, or a queue half full
	rateCutFlood   = 0.5  // the queue overflowed: the link carries much less than we send
	rateClimb      = 1.06
	rateProbe      = 1.01 // the climb near the rate the last trouble began at
	rateNearCeil   = 0.85 // from this share of that rate on, the climb is rateProbe
	rateCeilingTTL = 60   // clean ticks after which that rate is forgotten
	rateCooldown   = 2    // ticks after a cut before the next one
	rateCleanRun   = 4    // clean ticks before climbing back
	rateFloorKbps  = 600
	heavyDrops     = 8   // new drops in one tick that make it heavy
	floodDrops     = 100 // ... and that mean the queue overflowed
)

type sendRateState struct {
	kbps        float64
	lastDropped uint64
	at          time.Time
	sinceCut    int     // ticks since the last cut
	clean       int     // clean ticks in a row
	ceiling     float64 // the rate the last trouble began at; 0: none known
}

// congestion is how backed up the path is in one tick.
type congestion int

const (
	pathClean congestion = iota
	pathMild
	pathHeavy
	pathFlood
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
	case newDrops >= floodDrops:
		level = pathFlood
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
		enc  LocalEncoder
		hz   []int // the viewers' screens, for an encoder that follows them
	}
	var limits []limit
	for id, ann := range room.localStreams {
		share := r.kbps * float64(ann.BitrateKbps) / float64(capKbps)
		role := captureRole(ann.MediaType)
		l := limit{role, uint64(share * 1000), m.encoders[role], nil}
		if _, ok := l.enc.(ViewerScreens); ok {
			if sm, ok := room.StreamEngine.GetStreamManifest(id); ok {
				for _, sub := range sm.Subscribers {
					l.hz = append(l.hz, min(max(sm.Profiles[sub].DisplayHz, 0), tree.MaxDisplayHz))
				}
			}
		}
		limits = append(limits, l)
	}
	m.later(func() {
		for _, l := range limits {
			if l.enc != nil {
				l.enc.SetBitrate(l.bps)
				if v, ok := l.enc.(ViewerScreens); ok {
					v.SetViewerRates(l.hz)
				}
				continue
			}
			_ = br.SetSendBitrate(l.role, l.bps) // the bridge logs failures, rate limited
		}
	})
}

// next is one step of the rate; zero means no rate yet, which starts at the preset.
func (r *sendRateState) next(capKbps, floor float64, level congestion) float64 {
	kbps := r.kbps
	calm := r.clean // clean ticks before this one
	r.sinceCut++
	if level == pathClean {
		r.clean++
	} else {
		r.clean = 0
	}
	if r.clean >= rateCeilingTTL {
		r.ceiling = 0
	}
	switch {
	case kbps == 0:
		kbps = capKbps
	case level != pathClean && (r.sinceCut > rateCooldown || level == pathFlood):
		// the first cut of a trouble marks the rate the link could not carry; the cuts that follow
		// it within the same trouble do not lower that mark to wherever the trouble ended
		if r.ceiling == 0 || calm >= rateCleanRun {
			r.ceiling = kbps
		}
		cut := rateCutMild
		switch level {
		case pathHeavy:
			cut = rateCutHeavy
		case pathFlood:
			cut = rateCutFlood
		}
		kbps *= cut
		r.sinceCut = 0
	case level == pathClean && r.clean >= rateCleanRun:
		if r.ceiling > 0 && kbps >= r.ceiling*rateNearCeil {
			kbps *= rateProbe
		} else {
			kbps *= rateClimb
		}
	}
	return min(max(kbps, floor), capKbps)
}
