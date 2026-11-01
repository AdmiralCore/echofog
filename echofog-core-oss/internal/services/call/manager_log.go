package call

import (
	"log/slog"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Logging helpers of the call manager. Lifecycle events go to the log as they happen; anything
// that can repeat per packet or per tick goes through m.lim.

// statsLogInterval is how often an active call writes one line of media counters, so a log of a
// bad call shows when the trouble started even if nothing else was logged.
const statsLogInterval = 30 * time.Second

// inbound wraps a swarm frame handler: errors are logged here, rate limited per kind and peer,
// and not handed back to the swarm, which would log each one again without a limit.
func (m *CallManager) inbound(kind string, h func([]byte, types.PeerID) error) func([]byte, types.PeerID) error {
	return func(payload []byte, from types.PeerID) error {
		if err := h(payload, from); err != nil {
			m.lim.Log(m.logger, slog.LevelWarn, "inbound:"+kind+":"+from.String(), "Dropped incoming call frame",
				"kind", kind, "from", calllog.Peer(from), "err", err)
		}
		return nil
	}
}

// HandleRelayedFrame takes a call frame that came over a relay circuit instead of a direct link:
// from somebody behind a symmetric NAT, which no hole punch gets through. from is what the circuit
// claims; signals carry their own signature and media its room encryption, so a relay can neither
// forge nor read them.
func (m *CallManager) HandleRelayedFrame(t net_types.MessageType, payload []byte, from types.PeerID) {
	switch t {
	case net_types.TypeCallSignaling, net_types.TypeCallSignalingLossy:
		_ = m.inbound("signal", m.handleIncomingSignaling)(payload, from)
	case net_types.TypeCallMediaAudio:
		_ = m.inbound("audio", m.handleIncomingAudio)(payload, from)
	case net_types.TypeCallMediaVideo:
		_ = m.inbound("video", m.handleIncomingVideo)(payload, from)
	}
}

// SetFrameSender replaces how call frames reach peers (the node adds relay circuits for peers it
// has no direct link to). It must be set before a call starts.
func (m *CallManager) SetFrameSender(s media.FrameSender) {
	m.mu.Lock()
	m.sender = s
	m.mu.Unlock()
}

// warnOnce logs a rate-limited warning; key should name the event and whatever it is about.
func (m *CallManager) warnOnce(key, msg string, args ...any) {
	m.lim.Log(m.logger, slog.LevelWarn, key, msg, args...)
}

// debugOnce logs a rate-limited debug line.
func (m *CallManager) debugOnce(key, msg string, args ...any) {
	m.lim.Log(m.logger, slog.LevelDebug, key, msg, args...)
}

// mediaArgs turns the media counters into log attributes. With prev it reports what changed since
// then, so a periodic line shows the current rate of trouble, not the call's history.
func mediaArgs(s media.DispatcherStats, prev *media.DispatcherStats) []any {
	var p media.DispatcherStats
	if prev != nil {
		p = *prev
	}
	ob, po := s.Outbox, p.Outbox
	return []any{
		"audio_delivered", s.AudioDelivered - p.AudioDelivered,
		"video_delivered", s.VideoDelivered - p.VideoDelivered,
		"relayed", s.FragmentsRelayed - p.FragmentsRelayed,
		"nacked", s.NackedUnits - p.NackedUnits,
		"retransmitted", s.Retransmitted - p.Retransmitted,
		"fec_recovered", s.Recovered - p.Recovered,
		"incomplete", s.IncompleteUnits - p.IncompleteUnits,
		"decrypt_failed", s.DecryptFailures - p.DecryptFailures,
		"malformed", s.Malformed - p.Malformed,
		"sent", ob.Sent[0] + ob.Sent[1] + ob.Sent[2] - po.Sent[0] - po.Sent[1] - po.Sent[2],
		"dropped_control", ob.Dropped[media.ClassControl] - po.Dropped[media.ClassControl],
		"dropped_audio", ob.Dropped[media.ClassAudio] - po.Dropped[media.ClassAudio],
		"dropped_video", ob.Dropped[media.ClassVideo] - po.Dropped[media.ClassVideo],
		"send_failed", ob.Failed[0] + ob.Failed[1] + ob.Failed[2] - po.Failed[0] - po.Failed[1] - po.Failed[2],
	}
}

// statsTickLocked writes the periodic media line of an active call.
func (m *CallManager) statsTickLocked(room *ActiveRoomState, now time.Time) {
	if now.Sub(room.lastStatsLog) < statsLogInterval {
		return
	}
	room.lastStatsLog = now
	s := room.Dispatcher.Stats()
	args := append([]any{"call_id", calllog.Call(room.CallID), "members", len(room.ConnectedPeers),
		"streams", room.StreamEngine.ActiveStreams(), "send_kbps", int(room.sendRate.kbps)}, mediaArgs(s, &room.lastStats)...)
	room.lastStats = s
	m.logger.Info("Call media stats (last 30s)", args...)
}

// logSummaryLocked writes what the call did in total and per stream, when this node leaves it.
func (m *CallManager) logSummaryLocked(room *ActiveRoomState) {
	args := append([]any{"call_id", calllog.Call(room.CallID), "duration", time.Since(room.startedAt).Round(time.Second).String()},
		mediaArgs(room.Dispatcher.Stats(), nil)...)
	m.logger.Info("Call media summary", args...)
	for id, c := range room.Dispatcher.StreamStats() {
		m.logger.Info("Call stream summary", "call_id", calllog.Call(room.CallID), "stream", id,
			"units_sent", c.UnitsSent, "delivered", c.Delivered, "lost", c.Lost, "nacked", c.Nacked, "retransmitted", c.Retransmitted,
			"fec_recovered", c.Recovered, "sound_sent", c.SoundSent, "sound_delivered", c.SoundDelivered)
	}
}
