package call

import (
	"bytes"
	"log/slog"
	"slices"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Viewers' path reports travel up each stream's tree: every media.FeedbackInterval a viewer sends
// its parent its own measurement (media.StreamFeedback) with the latest of each viewer under it,
// so the broadcaster knows every viewer's path, a hop's interval later per level, and its rate
// control slows down for them (rate.go). A report older than feedbackTTL is a viewer that left or
// a link that lost it: it no longer counts.
//
// One viewer whose own path cannot carry the stream would otherwise hold every other viewer to
// what it gets. So the broadcaster grades each viewer's recent reports, and one that is heavy
// again and again while another viewer's path is clean is weak: its trouble is its own, not the
// broadcaster's link. The coordinator hangs it at the bottom of the tree, where its losses reach
// nobody it would have fed, and the rate control follows the other viewers, not it: it gets what
// its path lets through. It stays weak until its path has been free of heavy trouble for
// weakRelease. With no other viewer reporting, the stream follows it as any viewer.
const (
	feedbackTTL   = time.Second
	maxReportKbps = 1_000_000
	pathSamples   = 8 // grades kept per viewer, one per rate tick: four seconds
	weakHeavy     = 3 // of them heavy or worse make a viewer weak ...
	weakCleanPeer = 6 // ... while another viewer had this many clean or held
	weakRelease   = 30 * time.Second
)

// viewerReport is a viewer's last report on a stream.
type viewerReport struct {
	fb     media.Feedback
	viewer types.PeerID
	at     time.Time
}

// worse reports whether a describes a worse path than b: more congested, or as congested and
// carrying less.
func (a viewerReport) worse(b viewerReport) bool {
	if a.fb.Level != b.fb.Level {
		return a.fb.Level > b.fb.Level
	}
	return a.fb.Kbps > 0 && (b.fb.Kbps == 0 || a.fb.Kbps < b.fb.Kbps)
}

// viewerPath is what the broadcaster keeps of a viewer of its stream.
type viewerPath struct {
	grades    [pathSamples]media.Congestion
	n         int
	lastHeavy time.Time
	weak      bool
}

func (p *viewerPath) add(c media.Congestion, now time.Time) {
	p.grades[p.n%pathSamples] = c
	p.n++
	if c >= media.PathHeavy {
		p.lastHeavy = now
	}
}

// count returns how many of the kept grades pass ok, and how many are kept.
func (p *viewerPath) count(ok func(media.Congestion) bool) (hits, kept int) {
	kept = min(p.n, pathSamples)
	for _, c := range p.grades[:kept] {
		if ok(c) {
			hits++
		}
	}
	return hits, kept
}

// inSubtree reports whether viewer is from or hangs below it in a stream's tree.
func inSubtree(sm *tree.StreamManifest, from, viewer types.PeerID) bool {
	for i := 0; i <= len(sm.Subscribers); i++ {
		if viewer == from {
			return true
		}
		up, ok := sm.ParentMap[viewer]
		if !ok {
			return false
		}
		viewer = up
	}
	return false
}

// noteFeedbackLocked keeps a child's reports on a stream: its own and those of the viewers under
// it. A report from a peer that is not our child on it (a tree update crossing it), or about a
// viewer outside its subtree, is dropped.
func (m *CallManager) noteFeedbackLocked(room *ActiveRoomState, from types.PeerID, fp signaling.StreamFeedbackPayload, now time.Time) {
	sm, ok := room.StreamEngine.GetStreamManifest(fp.StreamID)
	if !ok || sm.ParentMap[from] != m.selfID || len(fp.Reports) > signaling.MaxFeedbackReports {
		m.debugOnce("fb-stranger:"+fp.StreamID+":"+from.String(), "Ignored path report from a peer that is not our child on the stream",
			"stream", fp.StreamID, "from", calllog.Peer(from))
		return
	}
	reps := room.feedback[fp.StreamID]
	if reps == nil {
		reps = make(map[types.PeerID]viewerReport)
		room.feedback[fp.StreamID] = reps
	}
	m.noteLayersLocked(room, from, fp, now)
	for _, r := range fp.Reports {
		if !inSubtree(sm, from, r.Viewer) {
			continue
		}
		reps[r.Viewer] = viewerReport{fb: media.Feedback{
			Level:   min(media.Congestion(r.Level), media.PathFlood),
			Kbps:    min(max(r.Kbps, 0), maxReportKbps),
			Loss:    min(max(float64(r.Loss), 0), 1),
			DelayMs: max(r.DelayMs, 0),
		}, viewer: r.Viewer, at: now}
	}
}

// freshFeedbackLocked returns the fresh reports of a stream's viewers, forgetting stale ones.
func (m *CallManager) freshFeedbackLocked(room *ActiveRoomState, streamID string, now time.Time) []viewerReport {
	var out []viewerReport
	for peer, r := range room.feedback[streamID] {
		if now.Sub(r.at) > feedbackTTL {
			delete(room.feedback[streamID], peer)
			continue
		}
		out = append(out, r)
	}
	return out
}

// worstFeedbackLocked returns the worst fresh report of a stream's viewers, if any.
func (m *CallManager) worstFeedbackLocked(room *ActiveRoomState, streamID string, now time.Time) (viewerReport, bool) {
	var worst viewerReport
	found := false
	for _, r := range m.freshFeedbackLocked(room, streamID, now) {
		if !found || r.worse(worst) {
			worst, found = r, true
		}
	}
	return worst, found
}

// viewersLocked grades the viewers of our own streams once a rate tick, finds the weak ones and
// tells the coordinator when that changes. It returns the worst report of the strong viewers and
// of the weak ones.
func (m *CallManager) viewersLocked(room *ActiveRoomState, now time.Time) (strong, weak viewerReport) {
	for id := range room.paths {
		if _, ok := room.localStreams[id]; !ok {
			delete(room.paths, id)
		}
	}
	for id := range room.localStreams {
		paths := room.paths[id]
		if paths == nil {
			paths = make(map[types.PeerID]*viewerPath)
			room.paths[id] = paths
		}
		fresh := m.freshFeedbackLocked(room, id, now)
		seen := make(map[types.PeerID]bool, len(fresh))
		for _, r := range fresh {
			p := paths[r.viewer]
			if p == nil {
				p = &viewerPath{}
				paths[r.viewer] = p
			}
			p.add(r.fb.Level, now)
			seen[r.viewer] = true
		}
		for v, p := range paths {
			if !seen[v] && now.Sub(p.lastHeavy) > weakRelease {
				delete(paths, v) // left, or went silent long ago
			}
		}
		// a viewer is weak when its trouble is its own: another viewer's path was fine meanwhile
		changed := false
		for v, p := range paths {
			heavy, _ := p.count(func(c media.Congestion) bool { return c >= media.PathHeavy })
			others := false
			for o, q := range paths {
				clean, kept := q.count(func(c media.Congestion) bool { return c <= media.PathHold })
				others = others || (o != v && kept == pathSamples && clean >= weakCleanPeer)
			}
			switch {
			case !p.weak && heavy >= weakHeavy && others:
				p.weak, changed = true, true
				m.logger.Info("A viewer's path cannot carry the stream: it moves to the bottom of the tree and no longer holds the others back",
					"stream", id, "viewer", calllog.Peer(v), "heavy_of", heavy)
			case p.weak && now.Sub(p.lastHeavy) > weakRelease:
				p.weak, changed = false, true
				m.logger.Info("A weak viewer's path carries the stream again", "stream", id, "viewer", calllog.Peer(v))
			}
		}
		if changed {
			var list []types.PeerID
			for v, p := range paths {
				if p.weak {
					list = append(list, v)
				}
			}
			slices.SortFunc(list, func(a, b types.PeerID) int { return bytes.Compare(a[:], b[:]) })
			m.toCoordinatorLocked(room, signaling.ActionStreamWeak, signaling.StreamWeakPayload{StreamID: id, Weak: list})
		}
		// a heavy viewer next to one that has been clean all along is suspect at once: the stream
		// does not follow it down in the seconds before it is found weak
		clean := func(except types.PeerID) bool {
			for o, q := range paths {
				ok, kept := q.count(func(c media.Congestion) bool { return c <= media.PathHold })
				if o != except && kept >= 2 && ok == kept {
					return true
				}
			}
			return false
		}
		for _, r := range fresh {
			p := paths[r.viewer]
			if p != nil && (p.weak || r.fb.Level >= media.PathHeavy && clean(r.viewer)) {
				if r.worse(weak) {
					weak = r
				}
			} else if r.worse(strong) {
				strong = r
			}
		}
	}
	return strong, weak
}

// weakLocked (coordinator) applies the broadcaster's list of weak viewers to its stream's tree.
func (m *CallManager) weakLocked(room *ActiveRoomState, from types.PeerID, wp signaling.StreamWeakPayload) {
	sm, ok := room.StreamEngine.GetStreamManifest(wp.StreamID)
	if !ok || sm.Broadcaster != from {
		m.debugOnce("weak-stranger:"+wp.StreamID+":"+from.String(), "Ignored weak viewers from a peer that does not broadcast the stream",
			"stream", wp.StreamID, "from", calllog.Peer(from))
		return
	}
	sm, changed, err := room.StreamEngine.SetWeak(wp.StreamID, wp.Weak)
	if err != nil || !changed {
		return
	}
	m.treeLog.Info("Weak viewers of a stream hang at the bottom of its tree", "stream", wp.StreamID, "weak", len(sm.Weak), "epoch", sm.Epoch)
	m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
	m.afterStreamTreeChangeLocked(room, sm)
}

// feedbackTickLocked measures the streams this node watches and reports up their trees.
func (m *CallManager) feedbackTickLocked(room *ActiveRoomState, now time.Time) {
	since := now.Sub(room.lastFeedback)
	if since < media.FeedbackInterval || room.Dispatcher == nil {
		return
	}
	room.lastFeedback = now
	own := room.Dispatcher.StreamFeedback(min(since, 2*media.FeedbackInterval))
	for id := range room.feedback {
		if _, ok := room.localStreams[id]; !ok && !room.watching[id] {
			delete(room.feedback, id)
		}
	}
	for id := range room.ownLevel {
		if _, ok := own[id]; !ok && !room.watching[id] {
			delete(room.ownLevel, id)
		}
	}
	for id, fb := range own {
		room.ownLevel[id] = fb.Level
	}
	ob := room.Dispatcher.Outbox()
	for id := range room.watching {
		if _, local := room.localStreams[id]; local {
			continue
		}
		up, ok := room.StreamEngine.GetUpstream(id, m.selfID)
		if !ok || ob == nil {
			continue
		}
		var reports []signaling.ViewerFeedback
		add := func(r viewerReport) {
			if len(reports) < signaling.MaxFeedbackReports {
				reports = append(reports, signaling.ViewerFeedback{Viewer: r.viewer, Level: uint8(r.fb.Level), Kbps: r.fb.Kbps,
					Loss: float32(r.fb.Loss), DelayMs: r.fb.DelayMs})
			}
			if r.fb.Level >= media.PathHeavy {
				m.lim.Log(m.logger, slog.LevelInfo, "fb-up:"+id+":"+r.viewer.String(), "Path to a viewer of a watched stream is congested",
					"stream", id, "viewer", calllog.Peer(r.viewer), "level", int(r.fb.Level), "loss", int(r.fb.Loss*1000)/10.0,
					"delay_ms", r.fb.DelayMs, "kbps", r.fb.Kbps)
			}
		}
		if fb, ok := own[id]; ok {
			add(viewerReport{fb: fb, viewer: m.selfID})
		}
		for _, r := range m.freshFeedbackLocked(room, id, now) {
			add(r)
		}
		if len(reports) == 0 {
			continue
		}
		b, err := m.encodeLocked(room, signaling.ActionStreamFeedback, signaling.StreamFeedbackPayload{StreamID: id, Reports: reports,
			Layered: m.layeredBelowLocked(room, id, now)})
		if err != nil {
			m.warnOnce("encode:feedback", "Failed to encode call signal", "action", signaling.ActionStreamFeedback, "err", err)
			continue
		}
		// a datagram on the audio lane: a late report is worth nothing, the next one comes soon
		ob.Send(up, media.ClassAudio, net_types.TypeCallSignalingLossy, m.sealSignalLocked(room, up, b))
	}
}
