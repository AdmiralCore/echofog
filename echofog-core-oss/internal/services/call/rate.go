package call

import (
	"log/slog"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// The browser encoder only learns about the loopback to the bridge, which never congests. This
// loop watches the real path (outbox drops and queue depth) and caps the encoder with REMB (an
// encoder outside the webview, through SetLocalEncoder): a little when the path starts to back
// up, more when it is full, by half when the queue overflowed, then it waits for the cut to show
// before cutting again, and climbs back only after the path has stayed clean for a while, never
// above the preset. A jumpy rate makes a jumpy picture: each cut costs the encoder frames or
// detail, so a single late frame is not worth one.
//
// The path beyond the queue is the viewers' to see: their reports (feedback.go) count like the
// queue's own state, the worst of them and the queue's together; a viewer found weak counts only
// when no other viewer reports. A viewer that loses a tenth of
// the stream or waits on a long queue also caps the rate at nine tenths of what reaches it, the
// rate its path carries: cutting by quarters from 12 Mbit/s took seconds to find a 4 Mbit/s
// link, each of them costing the viewer frames. A little loss or queueing holds the rate where
// it is.
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
	rateProbe      = 1.02 // the climb near the rate the last trouble began at
	rateNearCeil   = 0.85 // from this share of that rate on, the climb is rateProbe
	ratePastCeil   = 1.1  // ... up to this share, past which the rate is forgotten: the link carries more now
	rateCeilingTTL = 60   // clean ticks after which that rate is forgotten
	rateCooldown   = 2    // ticks after a cut before the next one
	rateCleanRun   = 4    // clean ticks before climbing back
	rateFloorKbps  = 600
	rateObserved   = 0.9 // of what reaches a viewer whose path is heavy
	heavyDrops     = 8   // new drops in one tick that make it heavy
	floodDrops     = 100 // ... and that mean the queue overflowed
)

// sendRateState is the send rate control's memory, per room: the rate now, and what it needs to cut
// and climb with sense (see next).
type sendRateState struct {
	kbps        float64
	lastDropped uint64
	at          time.Time
	sinceCut    int     // ticks since the last cut
	clean       int     // clean ticks in a row
	ceiling     float64 // the rate the last trouble began at; 0: none known
}

// congestion is how backed up the path is in one tick: the viewers' grades and the queue's are
// the same scale.
type congestion = media.Congestion

const (
	pathClean = media.PathClean
	pathHold  = media.PathHold
	pathMild  = media.PathMild
	pathHeavy = media.PathHeavy
	pathFlood = media.PathFlood
)

// rateTickLocked runs the rate control once per rateInterval: it grades the path (our own queue and
// the viewers' reports), takes one step of the rate (next) and hands the result to each stream's
// encoder: the desktop's own encoder through SetBitrate, the browser's through REMB. m.mu must be held.
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
	queue := level
	viewer, weak := m.viewersLocked(room, now)
	if viewer.viewer == (types.PeerID{}) && !m.otherViewerLocked(room, weak.viewer, now) {
		viewer = weak // the weak viewer is the one viewer: the stream is theirs to follow
	}
	// A viewer's flood (a stall of a moment, its backlog arriving at once) is a heavy cut with the
	// cooldown; only the own queue overflowing skips the cooldown, or one hiccup halves the rate twice.
	level = max(level, min(viewer.fb.Level, pathHeavy))
	floor := float64(rateFloorKbps * len(room.localStreams))
	r.kbps = r.next(float64(capKbps), floor, level)
	if viewer.fb.Level >= pathHeavy && viewer.fb.Kbps > 0 {
		r.kbps = max(min(r.kbps, rateObserved*float64(viewer.fb.Kbps)), floor)
	}
	if queue >= pathMild {
		m.debugOnce("congested", "Upload path congested, lowering the send bitrate",
			"kbps", int(r.kbps), "cap_kbps", capKbps, "new_drops", newDrops, "backlog", backlog)
	}
	if viewer.fb.Level >= pathMild {
		m.lim.Log(m.logger, slog.LevelInfo, "viewer-congested", "A viewer's path is congested, lowering the send bitrate",
			"kbps", int(r.kbps), "viewer", calllog.Peer(viewer.viewer), "level", int(viewer.fb.Level),
			"loss", int(viewer.fb.Loss*1000)/10.0, "delay_ms", viewer.fb.DelayMs, "viewer_kbps", viewer.fb.Kbps)
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
	if level == pathClean { // a hold keeps the rate: no climb, no cut
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
	case level >= pathMild && (r.sinceCut > rateCooldown || level == pathFlood):
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
		if r.ceiling > 0 && kbps > r.ceiling*ratePastCeil {
			r.ceiling = 0
		}
		if r.ceiling > 0 && kbps >= r.ceiling*rateNearCeil {
			kbps *= rateProbe
		} else {
			kbps *= rateClimb
		}
	}
	return min(max(kbps, floor), capKbps)
}
