package call

import (
	"bytes"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// ---------------------------------------------------------------------------------------------
// Inbound frames

func (m *CallManager) handleIncomingSignaling(payload []byte, from types.PeerID) error {
	env, err := signaling.DecodeSignal(payload)
	if err != nil {
		return err
	}
	if m.signer != nil {
		if err := env.Verify(); err != nil {
			return err
		}
	}
	flooded := signaling.IsFlooded(env.Action)
	if !flooded && from != env.Sender {
		return ErrSpoofedSender
	}

	m.mu.Lock()
	room := m.activeRoom
	if room == nil || room.CallID != env.CallID || env.Sender == m.selfID {
		m.mu.Unlock()
		return nil
	}

	if flooded {
		key := floodKey{env.Sender, env.MsgID}
		if _, dup := room.seen[key]; dup {
			m.mu.Unlock()
			return nil
		}
		room.seen[key] = time.Now()
		if m.applyFloodLocked(room, env, from) {
			for _, n := range room.Dispatcher.VoiceNeighbors() {
				if n != from && n != env.Sender {
					room.Dispatcher.Outbox().Send(n, media.ClassControl, net_types.TypeCallSignaling, payload)
				}
			}
		}
	} else {
		m.applyUnicastLocked(room, env)
	}
	m.unlockAndFlush()
	return nil
}

// authorityLocked decides whether a coordinator claim (sender at epoch) supersedes ours.
// Higher epoch wins; on a tie between two coordinators the lower peer ID wins, so a split
// brain converges to one coordinator.
func (m *CallManager) authorityLocked(room *ActiveRoomState, sender types.PeerID, epoch uint64) bool {
	if sender == room.Coordinator {
		return epoch >= room.Epoch
	}
	if epoch != room.Epoch {
		return epoch > room.Epoch
	}
	return bytes.Compare(sender[:], room.Coordinator[:]) < 0
}

func (m *CallManager) applyFloodLocked(room *ActiveRoomState, env *signaling.CallSignalEnvelope, from types.PeerID) bool {
	now := time.Now()
	switch env.Action {
	case signaling.ActionTreeManifest:
		var manifest tree.TreeManifest
		if decodePayload(env.Payload, &manifest) != nil || manifest.Coordinator != env.Sender || manifest.CallID != room.CallID {
			return false
		}
		if !m.authorityLocked(room, env.Sender, manifest.Epoch) {
			return false
		}
		m.applyVoiceManifestLocked(room, &manifest, now)
		return true

	case signaling.ActionCoordinatorHeartbeat:
		if env.Sender != room.Coordinator {
			if !m.authorityLocked(room, env.Sender, env.Epoch) {
				return false
			}
			m.setCoordinatorLocked(room, env.Sender, env.Epoch, now)
		}
		room.LastCoordPulse = now
		if from == m.upstreamLocked(room) {
			room.LastTreePulse = now
		}
		var hb signaling.CoordinatorHeartbeatPayload
		if decodePayload(env.Payload, &hb) == nil && m.replicaStaleLocked(room, hb) && now.Sub(room.lastSyncReq) >= syncRequestInterval {
			room.lastSyncReq = now
			m.sendLocked(room, room.Coordinator, signaling.ActionSyncRequest, nil)
		}
		return true

	case signaling.ActionCoordinatorElection:
		var leader types.PeerID
		if decodePayload(env.Payload, &leader) != nil || leader != env.Sender {
			return false // only the winner may announce its own election
		}
		if !m.authorityLocked(room, leader, env.Epoch) {
			return false
		}
		m.setCoordinatorLocked(room, leader, env.Epoch, now)
		return true

	case signaling.ActionStreamAnnounce:
		var ann signaling.StreamAnnouncePayload
		if env.Sender != room.Coordinator || decodePayload(env.Payload, &ann) != nil {
			return false
		}
		room.announces[ann.StreamID] = ann
		m.notifyAnnounceLocked(ann)
		return true

	case signaling.ActionStreamTreeUpdate:
		var sm tree.StreamManifest
		if env.Sender != room.Coordinator || decodePayload(env.Payload, &sm) != nil {
			return false
		}
		m.applyStreamManifestLocked(room, &sm)
		return true

	case signaling.ActionStreamClose:
		var streamID string
		if env.Sender != room.Coordinator || decodePayload(env.Payload, &streamID) != nil {
			return false
		}
		m.closeStreamLocalLocked(room, streamID)
		return true

	case signaling.ActionVADState:
		var vad signaling.VADPayload
		if decodePayload(env.Payload, &vad) != nil || vad.Speaker != env.Sender {
			return false
		}
		if cb := m.OnActiveSpeaker; cb != nil {
			m.later(func() { cb(vad.Speaker, vad.Speaking, vad.AudioDb) })
		}
		return true
	}
	return false
}

func (m *CallManager) applyUnicastLocked(room *ActiveRoomState, env *signaling.CallSignalEnvelope) {
	now := time.Now()
	sender := env.Sender
	switch env.Action {
	case signaling.ActionJoin, signaling.ActionMemberHeartbeat:
		if !room.IsCoordinator {
			return
		}
		var jp signaling.JoinPayload
		if decodePayload(env.Payload, &jp) != nil {
			return
		}
		if _, member := room.ConnectedPeers[sender]; member && env.Action == signaling.ActionMemberHeartbeat {
			room.memberSeen[sender] = now
			return
		}
		if room.banned[sender] {
			return
		}
		if !signaling.VerifyJoinProof(room.RoomKey, room.CallID, sender, jp.Proof) {
			m.logger.Warn("Rejected call join without valid room key proof", "peer", sender.String())
			return
		}
		m.admitLocked(room, sender, jp.Profile, now)

	case signaling.ActionLeave:
		if room.IsCoordinator {
			if _, member := room.ConnectedPeers[sender]; member {
				m.removeMemberLocked(room, sender)
			}
		} else if sender == room.Coordinator {
			room.LastCoordPulse = time.Time{} // coordinator left: elect right away
			room.suspects[sender] = true
		}

	case signaling.ActionParentLost:
		var parent types.PeerID
		if !room.IsCoordinator || decodePayload(env.Payload, &parent) != nil || parent == m.selfID {
			return
		}
		if _, ok := room.memberSeen[parent]; ok {
			// confirmed on the next ticks once the parent's own heartbeats stop too
			if _, already := room.reportedLost[parent]; !already {
				room.reportedLost[parent] = now
			}
		}

	case signaling.ActionStreamRegister:
		var ann signaling.StreamAnnouncePayload
		if room.IsCoordinator && decodePayload(env.Payload, &ann) == nil && ann.Broadcaster == sender {
			m.registerStreamLocked(room, ann)
		}

	case signaling.ActionStreamUnregister:
		var streamID string
		if room.IsCoordinator && decodePayload(env.Payload, &streamID) == nil {
			m.unregisterStreamLocked(room, streamID, sender)
		}

	case signaling.ActionStreamSubscribe:
		var sub signaling.StreamSubscribePayload
		if room.IsCoordinator && decodePayload(env.Payload, &sub) == nil && sub.Subscriber == sender {
			m.subscribeLocked(room, sub)
		}

	case signaling.ActionStreamUnsub:
		var streamID string
		if room.IsCoordinator && decodePayload(env.Payload, &streamID) == nil {
			m.unsubscribeLocked(room, streamID, sender)
		}

	case signaling.ActionPing:
		m.sendLocked(room, sender, signaling.ActionPong, nil)

	case signaling.ActionStreamReparent:
		var rp signaling.ReparentPayload
		if room.IsCoordinator && decodePayload(env.Payload, &rp) == nil {
			m.reparentLocked(room, sender, rp, now)
		}

	case signaling.ActionReport:
		var rp signaling.ReportPayload
		if room.IsCoordinator && decodePayload(env.Payload, &rp) == nil {
			m.reportLocked(room, sender, rp, now)
		}

	case signaling.ActionKicked:
		if sender == room.Coordinator {
			var reason string
			_ = decodePayload(env.Payload, &reason)
			m.logger.Warn("Removed from call by the room", "reason", reason)
			m.emit("call:kicked", map[string]any{"call_id": room.CallID, "reason": reason})
			cb := m.OnKicked
			m.later(func() {
				go func() { _ = m.LeaveRoom() }()
				if cb != nil {
					cb(reason)
				}
			})
		}

	case signaling.ActionSyncRequest:
		if _, member := room.ConnectedPeers[sender]; room.IsCoordinator && member {
			m.sendRoomSyncLocked(room, sender)
		}

	case signaling.ActionPong:
		if sender == room.Coordinator {
			room.LastCoordPulse = now
		}

	case signaling.ActionRoomSync:
		var rs signaling.RoomSyncPayload
		if decodePayload(env.Payload, &rs) != nil || rs.Voice == nil || rs.Voice.Coordinator != sender {
			return
		}
		if !m.authorityLocked(room, sender, rs.Voice.Epoch) {
			return
		}
		if rs.Voice.Epoch > room.Epoch || rs.Voice.Coordinator != room.Coordinator || room.VoiceManifest == nil {
			m.applyVoiceManifestLocked(room, rs.Voice, now)
		}
		live := make(map[string]bool, len(rs.Streams))
		for _, sm := range rs.Streams {
			live[sm.StreamID] = true
			m.applyStreamManifestLocked(room, sm)
		}
		for _, id := range room.StreamEngine.ActiveStreams() {
			if !live[id] {
				m.closeStreamLocalLocked(room, id) // its close flood never reached us
			}
		}
		for _, ann := range rs.Announces {
			if _, known := room.announces[ann.StreamID]; !known {
				room.announces[ann.StreamID] = ann
				m.notifyAnnounceLocked(ann)
			}
		}

	case signaling.ActionAdopt:
		if _, member := room.ConnectedPeers[sender]; member {
			room.Dispatcher.Adopt(sender)
			m.ensureConnectedLocked(sender)
		}

	case signaling.ActionNack:
		var np signaling.NackPayload
		if decodePayload(env.Payload, &np) == nil {
			if up := room.Dispatcher.ServeNack(sender, np.StreamID, np.Lost); len(up) > 0 {
				m.nackUpstreamLocked(room, np.StreamID, up)
			}
		}

	case signaling.ActionKeyframeRequest:
		var streamID string
		if decodePayload(env.Payload, &streamID) == nil {
			m.handleKeyframeRequestLocked(room, streamID, now)
		}
	}
}

func (m *CallManager) sendJoinLocked(room *ActiveRoomState, now time.Time) {
	room.lastJoin = now
	m.sendLocked(room, room.Coordinator, signaling.ActionJoin, m.joinPayloadLocked(room))
}

func (m *CallManager) joinPayloadLocked(room *ActiveRoomState) signaling.JoinPayload {
	return signaling.JoinPayload{Profile: room.MyProfile, Proof: signaling.JoinProof(room.RoomKey, room.CallID, m.selfID)}
}

// toCoordinatorLocked routes a request to the coordinator, or handles it in place if we are it.
func (m *CallManager) toCoordinatorLocked(room *ActiveRoomState, action signaling.SignalAction, data any) {
	if !room.IsCoordinator {
		m.sendLocked(room, room.Coordinator, action, data)
		return
	}
	switch action {
	case signaling.ActionStreamRegister:
		m.registerStreamLocked(room, data.(signaling.StreamAnnouncePayload))
	case signaling.ActionStreamUnregister:
		m.unregisterStreamLocked(room, data.(string), m.selfID)
	case signaling.ActionStreamSubscribe:
		m.subscribeLocked(room, data.(signaling.StreamSubscribePayload))
	case signaling.ActionStreamUnsub:
		m.unsubscribeLocked(room, data.(string), m.selfID)
	case signaling.ActionStreamReparent:
		m.reparentLocked(room, m.selfID, data.(signaling.ReparentPayload), time.Now())
	case signaling.ActionReport:
		m.reportLocked(room, m.selfID, data.(signaling.ReportPayload), time.Now())
	}
}

// ---------------------------------------------------------------------------------------------
// Plumbing

func (m *CallManager) encodeLocked(room *ActiveRoomState, action signaling.SignalAction, data any) ([]byte, error) {
	return signaling.EncodeSignedSignal(m.signer, action, room.CallID, m.selfID, room.Epoch, data)
}

func (m *CallManager) sendLocked(room *ActiveRoomState, to types.PeerID, action signaling.SignalAction, data any) {
	if to == m.selfID || to == (types.PeerID{}) {
		return
	}
	b, err := m.encodeLocked(room, action, data)
	if err != nil {
		m.logger.Warn("Failed to encode call signal", "action", action, "err", err)
		return
	}
	room.Dispatcher.Outbox().Send(to, media.ClassControl, net_types.TypeCallSignaling, b)
}

func (m *CallManager) floodLocked(room *ActiveRoomState, action signaling.SignalAction, data any) {
	b, err := m.encodeLocked(room, action, data)
	if err != nil {
		m.logger.Warn("Failed to encode call signal", "action", action, "err", err)
		return
	}
	for _, n := range room.Dispatcher.VoiceNeighbors() {
		room.Dispatcher.Outbox().Send(n, media.ClassControl, net_types.TypeCallSignaling, b)
	}
}

func (m *CallManager) notifyAnnounceLocked(a signaling.StreamAnnouncePayload) {
	onAnn, onInfo := m.OnStreamAnnounced, m.OnStreamInfo
	if onAnn == nil && onInfo == nil {
		return
	}
	m.later(func() {
		if onAnn != nil {
			onAnn(a.StreamID, a.Broadcaster, a.MediaType, a.BitrateKbps)
		}
		if onInfo != nil {
			onInfo(a)
		}
	})
}
