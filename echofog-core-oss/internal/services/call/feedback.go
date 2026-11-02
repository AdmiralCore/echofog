package call

import (
	"log/slog"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Viewers' path reports travel up each stream's tree: every media.FeedbackInterval a viewer sends
// its parent the worse of its own measurement (media.StreamFeedback) and its children's reports,
// so the broadcaster hears the worst path of the whole tree, a hop's interval later per level,
// and its rate control slows down for it (rate.go). A report older than feedbackTTL is a viewer
// that left or a link that lost it: it no longer counts.
const (
	feedbackTTL   = time.Second
	maxReportKbps = 1_000_000
)

// viewerReport is a child's last report on a stream.
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

// noteFeedbackLocked keeps a child's report on a stream; a report from a peer that is not our
// child on it (a tree update crossing it) is dropped.
func (m *CallManager) noteFeedbackLocked(room *ActiveRoomState, from types.PeerID, fp signaling.StreamFeedbackPayload, now time.Time) {
	child := false
	for _, c := range room.StreamEngine.GetDownstream(fp.StreamID, m.selfID) {
		child = child || c == from
	}
	if !child {
		m.debugOnce("fb-stranger:"+fp.StreamID+":"+from.String(), "Ignored path report from a peer that is not our child on the stream",
			"stream", fp.StreamID, "from", calllog.Peer(from))
		return
	}
	reps := room.feedback[fp.StreamID]
	if reps == nil {
		reps = make(map[types.PeerID]viewerReport)
		room.feedback[fp.StreamID] = reps
	}
	viewer := fp.Viewer
	if viewer == (types.PeerID{}) {
		viewer = from
	}
	reps[from] = viewerReport{fb: media.Feedback{
		Level:   min(media.Congestion(fp.Level), media.PathFlood),
		Kbps:    min(max(fp.Kbps, 0), maxReportKbps),
		Loss:    min(max(float64(fp.Loss), 0), 1),
		DelayMs: max(fp.DelayMs, 0),
	}, viewer: viewer, at: now}
}

// worstFeedbackLocked returns the worst fresh report of a stream's children, if any.
func (m *CallManager) worstFeedbackLocked(room *ActiveRoomState, streamID string, now time.Time) (viewerReport, bool) {
	var worst viewerReport
	found := false
	for peer, r := range room.feedback[streamID] {
		if now.Sub(r.at) > feedbackTTL {
			delete(room.feedback[streamID], peer)
			continue
		}
		if !found || r.worse(worst) {
			worst, found = r, true
		}
	}
	return worst, found
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
	ob := room.Dispatcher.Outbox()
	for id := range room.watching {
		if _, local := room.localStreams[id]; local {
			continue
		}
		up, ok := room.StreamEngine.GetUpstream(id, m.selfID)
		if !ok || ob == nil {
			continue
		}
		rep, have := viewerReport{viewer: m.selfID, at: now}, false
		if fb, ok := own[id]; ok {
			rep.fb, have = fb, true
		}
		if child, ok := m.worstFeedbackLocked(room, id, now); ok && (!have || child.worse(rep)) {
			rep, have = child, true
		}
		if !have {
			continue
		}
		if rep.fb.Level >= media.PathHeavy {
			m.lim.Log(m.logger, slog.LevelInfo, "fb-up:"+id, "Path to a viewer of a watched stream is congested",
				"stream", id, "viewer", calllog.Peer(rep.viewer), "level", int(rep.fb.Level), "loss", int(rep.fb.Loss*1000)/10.0,
				"delay_ms", rep.fb.DelayMs, "kbps", rep.fb.Kbps)
		}
		b, err := m.encodeLocked(room, signaling.ActionStreamFeedback, signaling.StreamFeedbackPayload{
			StreamID: id, Level: uint8(rep.fb.Level), Kbps: rep.fb.Kbps, Loss: float32(rep.fb.Loss), DelayMs: rep.fb.DelayMs, Viewer: rep.viewer,
		})
		if err != nil {
			m.warnOnce("encode:feedback", "Failed to encode call signal", "action", signaling.ActionStreamFeedback, "err", err)
			continue
		}
		// a datagram on the audio lane: a late report is worth nothing, the next one comes soon
		ob.Send(up, media.ClassAudio, net_types.TypeCallSignalingLossy, b)
	}
}
