package call

import (
	"bytes"
	"fmt"
	"log/slog"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// ---------------------------------------------------------------------------------------------
// Inbound frames

func (m *CallManager) handleIncomingSignaling(payload []byte, from types.PeerID) error {
	if isSealedSignal(payload) {
		m.mu.Lock()
		inner, err := m.openSignalLocked(m.activeRoom, from, payload)
		m.mu.Unlock()
		if err != nil {
			return err
		}
		payload = inner // the envelope its originator signed, as if it had come plain
	}
	env, err := signaling.DecodeSignal(payload)
	if err != nil {
		return err
	}
	if m.signer != nil {
		if err := env.Verify(); err != nil {
			return fmt.Errorf("%s from %s: %w", env.Action, calllog.Peer(env.Sender), err)
		}
	}
	flooded := signaling.IsFlooded(env.Action)
	if !flooded && from != env.Sender {
		return fmt.Errorf("%s claims sender %s: %w", env.Action, calllog.Peer(env.Sender), ErrSpoofedSender)
	}

	m.mu.Lock()
	room := m.activeRoom
	if room == nil || room.CallID != env.CallID || env.Sender == m.selfID {
		m.mu.Unlock()
		switch {
		case room == nil:
			// common for a moment after leaving: the room has not noticed yet
			m.debugOnce("noroom:"+from.String(), "Ignored call signal while not in a call",
				"action", env.Action, "from", calllog.Peer(from), "call_id", calllog.Call(env.CallID))
		case env.Sender != m.selfID:
			m.debugOnce("othercall:"+from.String(), "Ignored call signal for another call",
				"action", env.Action, "from", calllog.Peer(from), "call_id", calllog.Call(env.CallID))
		}
		return nil
	}

	if flooded {
		key := floodKey{env.Sender, env.MsgID}
		if _, dup := room.seen[key]; dup {
			m.mu.Unlock()
			return nil
		}
		room.seen[key] = m.now()
		if m.applyFloodLocked(room, env, from) {
			for _, n := range room.Dispatcher.VoiceNeighbors() {
				if n != from && n != env.Sender {
					m.sendRawLocked(room, n, env.Action, payload)
				}
			}
		}
	} else {
		m.applyUnicastLocked(room, env)
	}
	m.unlockAndFlush()
	return nil
}

// rejectLocked logs a signal that was dropped and returns false. warn marks signals that should
// never happen between honest, up to date peers (malformed or forged); the rest are usually stale
// messages crossing a failover and go to Debug.
func (m *CallManager) rejectLocked(room *ActiveRoomState, env *signaling.CallSignalEnvelope, reason string, warn bool) bool {
	level := slog.LevelDebug
	if warn {
		level = slog.LevelWarn
	}
	m.lim.Log(m.logger, level, "reject:"+string(env.Action)+":"+reason+":"+env.Sender.String(), "Rejected call signal",
		"action", env.Action, "reason", reason, "sender", calllog.Peer(env.Sender), "epoch", env.Epoch,
		"our_epoch", room.Epoch, "coordinator", calllog.Peer(room.Coordinator))
	return false
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

// knownInCallLocked reports whether the call knows id as one of its participants: the coordinator
// (present or the one before), a connected member, or a member or standby the voice tree names,
// and not banned from the call. Only such a peer may claim the call's lead: a signature proves who
// sent a claim, not that they belong to the call, and anyone who learnt the call's ID could
// otherwise announce a higher epoch and take the tree over.
func (m *CallManager) knownInCallLocked(room *ActiveRoomState, id types.PeerID) bool {
	if room.banned[id] {
		return false
	}
	if id == room.Coordinator || (id == room.formerCoordinator && id != (types.PeerID{})) {
		return true
	}
	if _, ok := room.ConnectedPeers[id]; ok {
		return true
	}
	if _, ok := room.timedOut[id]; ok {
		// a member lost to a timeout or a silent parent is not a stranger: it may come back, and
		// a claim of its own is one a half we kept must answer
		return true
	}
	if mf := room.VoiceManifest; mf != nil {
		if id == mf.Coordinator {
			return true
		}
		for _, s := range mf.StandbyCoordinators {
			if s == id {
				return true
			}
		}
		if _, ok := mf.ParentMap[id]; ok {
			return true
		}
	}
	return false
}

func (m *CallManager) applyFloodLocked(room *ActiveRoomState, env *signaling.CallSignalEnvelope, from types.PeerID) bool {
	now := m.now()
	switch env.Action {
	case signaling.ActionTreeManifest:
		var manifest tree.TreeManifest
		if decodePayload(env.Payload, &manifest) != nil {
			return m.rejectLocked(room, env, "malformed", true)
		}
		if manifest.Coordinator != env.Sender || manifest.CallID != room.CallID {
			return m.rejectLocked(room, env, "manifest not signed by its coordinator", true)
		}
		if !m.knownInCallLocked(room, env.Sender) {
			return m.rejectLocked(room, env, "manifest from outside the call", true)
		}
		if !m.authorityLocked(room, env.Sender, manifest.Epoch) {
			return m.rejectLocked(room, env, "stale epoch", false)
		}
		m.applyVoiceManifestLocked(room, &manifest, now)
		return true

	case signaling.ActionCoordinatorHeartbeat:
		if env.Sender != room.Coordinator {
			known := m.knownInCallLocked(room, env.Sender)
			if !known {
				// a flooded claim our own coordinator forwarded to us is one it took: the half
				// it joined is the call, and this is how its members learn where it went.
				vouched := from == room.Coordinator && m.knownInCallLocked(room, from)
				if !vouched {
					return m.rejectLocked(room, env, "heartbeat from outside the call", true)
				}
			}
			var other signaling.CoordinatorHeartbeatPayload
			read := decodePayload(env.Payload, &other) == nil
			switch {
			case m.aloneLocked(room) && other.Members == 1 && !m.authorityLocked(room, env.Sender, env.Epoch):
				// two coordinators, each alone (a blackout split the call): the one with the
				// higher epoch, or the lower ID on a tie, keeps it; that one is us, and the other
				// joins us when it hears our heartbeat
				return m.rejectLocked(room, env, "another lone coordinator yields to us", false)
			case m.aloneLocked(room):
				// we took over while cut off and nobody followed: the sender has the call
				m.yieldLocked(room, env.Sender, env.Epoch, now)
			case read && other.Members > 0 && other.Members < len(room.ConnectedPeers):
				// an election ran on a node cut off from the call: its half is smaller than the
				// one we kept. The smaller half yields; taking its claim would make the two
				// halves swap the lead and the call would not become one room again. Answer with
				// our own heartbeat, which reaches the half it leads: floods do not cross the
				// split, but this one is sent to the claimant directly.
				if room.IsCoordinator {
					m.sendLocked(room, env.Sender, signaling.ActionCoordinatorHeartbeat, m.coordinatorHeartbeatLocked(room))
				}
				return m.rejectLocked(room, env, "a smaller half yields to us", false)
			case read && other.Members > len(room.ConnectedPeers):
				// the sender kept strictly more members: its half is the call, ours is the
				// splinter. A member follows the larger half; a coordinator of the smaller one
				// yields to it and asks to be taken in, whatever its epoch.
				if room.IsCoordinator {
					m.yieldLocked(room, env.Sender, env.Epoch, now)
				} else {
					m.setCoordinatorLocked(room, env.Sender, env.Epoch, now)
				}
			case !m.authorityLocked(room, env.Sender, env.Epoch):
				return m.rejectLocked(room, env, "heartbeat from a superseded coordinator", false)
			default:
				m.setCoordinatorLocked(room, env.Sender, env.Epoch, now)
			}
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
			return m.rejectLocked(room, env, "election not announced by its winner", true) // only the winner may announce its own election
		}
		if !m.knownInCallLocked(room, leader) {
			return m.rejectLocked(room, env, "election won outside the call", true)
		}
		if !m.authorityLocked(room, leader, env.Epoch) {
			return m.rejectLocked(room, env, "stale epoch", false)
		}
		m.setCoordinatorLocked(room, leader, env.Epoch, now)
		return true

	case signaling.ActionStreamAnnounce:
		var ann signaling.StreamAnnouncePayload
		if env.Sender != room.Coordinator {
			return m.rejectLocked(room, env, "not from the coordinator", false)
		}
		if decodePayload(env.Payload, &ann) != nil {
			return m.rejectLocked(room, env, "malformed", true)
		}
		if _, known := room.announces[ann.StreamID]; !known {
			m.logger.Info("Stream announced", "stream", ann.StreamID, "broadcaster", calllog.Peer(ann.Broadcaster),
				"media", ann.MediaType, "quality", ann.QualityID, "kbps", ann.BitrateKbps)
		}
		room.announces[ann.StreamID] = ann
		m.notifyAnnounceLocked(ann)
		return true

	case signaling.ActionStreamTreeUpdate:
		var sm tree.StreamManifest
		if env.Sender != room.Coordinator {
			return m.rejectLocked(room, env, "not from the coordinator", false)
		}
		if decodePayload(env.Payload, &sm) != nil {
			return m.rejectLocked(room, env, "malformed", true)
		}
		m.applyStreamManifestLocked(room, &sm)
		return true

	case signaling.ActionStreamClose:
		var streamID string
		if env.Sender != room.Coordinator {
			return m.rejectLocked(room, env, "not from the coordinator", false)
		}
		if decodePayload(env.Payload, &streamID) != nil {
			return m.rejectLocked(room, env, "malformed", true)
		}
		m.closeStreamLocalLocked(room, streamID)
		return true

	case signaling.ActionVADState:
		var vad signaling.VADPayload
		if decodePayload(env.Payload, &vad) != nil || vad.Speaker != env.Sender {
			return m.rejectLocked(room, env, "malformed or about another speaker", true)
		}
		if cb := m.OnActiveSpeaker; cb != nil {
			m.later(func() { cb(vad.Speaker, vad.Speaking, vad.AudioDb) })
		}
		return true
	}
	return m.rejectLocked(room, env, "unknown flooded action", false)
}

func (m *CallManager) applyUnicastLocked(room *ActiveRoomState, env *signaling.CallSignalEnvelope) {
	now := m.now()
	sender := env.Sender
	switch env.Action {
	case signaling.ActionJoin, signaling.ActionMemberHeartbeat:
		if !room.IsCoordinator {
			// a member that still thinks we lead (or an election candidate): it retries elsewhere
			m.rejectLocked(room, env, "not the coordinator", false)
			return
		}
		var jp signaling.JoinPayload
		if decodePayload(env.Payload, &jp) != nil {
			m.rejectLocked(room, env, "malformed", true)
			return
		}
		if _, member := room.ConnectedPeers[sender]; member && env.Action == signaling.ActionMemberHeartbeat {
			room.memberSeen[sender] = now
			m.noteBackdropLocked(room, sender, jp.Profile.Backdrop)
			m.noteKXLocked(room, sender, jp.KX, jp.SK, now)
			return
		}
		if room.banned[sender] {
			m.rejectLocked(room, env, "banned", true)
			return
		}
		if !signaling.VerifyJoinProof(room.RoomKey, room.CallID, sender, jp.Proof) {
			m.warnOnce("badproof:"+sender.String(), "Rejected call join without valid room key proof", "peer", calllog.Peer(sender))
			return
		}
		m.noteKXLocked(room, sender, jp.KX, jp.SK, now)
		m.admitLocked(room, sender, jp.Profile, now)

	case signaling.ActionLeave:
		if room.IsCoordinator {
			if _, member := room.ConnectedPeers[sender]; member {
				m.removeMemberLocked(room, sender, "left")
			}
		} else if sender == room.Coordinator {
			m.logger.Info("Call coordinator left, electing a new one", "coordinator", calllog.Peer(sender))
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
				m.treeLog.Info("Member reported its voice parent silent", "reporter", calllog.Peer(sender), "parent", calllog.Peer(parent))
				room.reportedLost[parent] = now
			}
		}

	case signaling.ActionStreamRegister:
		var ann signaling.StreamAnnouncePayload
		if !room.IsCoordinator {
			m.rejectLocked(room, env, "not the coordinator", false)
		} else if decodePayload(env.Payload, &ann) != nil || ann.Broadcaster != sender {
			m.rejectLocked(room, env, "malformed or for another broadcaster", true)
		} else {
			m.registerStreamLocked(room, ann)
		}

	case signaling.ActionStreamUnregister:
		var streamID string
		if !room.IsCoordinator {
			m.rejectLocked(room, env, "not the coordinator", false)
		} else if decodePayload(env.Payload, &streamID) != nil {
			m.rejectLocked(room, env, "malformed", true)
		} else {
			m.unregisterStreamLocked(room, streamID, sender)
		}

	case signaling.ActionStreamSubscribe:
		var sub signaling.StreamSubscribePayload
		if !room.IsCoordinator {
			m.rejectLocked(room, env, "not the coordinator", false)
		} else if decodePayload(env.Payload, &sub) != nil || sub.Subscriber != sender {
			m.rejectLocked(room, env, "malformed or for another subscriber", true)
		} else {
			m.subscribeLocked(room, sub)
		}

	case signaling.ActionStreamUnsub:
		var streamID string
		if !room.IsCoordinator {
			m.rejectLocked(room, env, "not the coordinator", false)
		} else if decodePayload(env.Payload, &streamID) != nil {
			m.rejectLocked(room, env, "malformed", true)
		} else {
			m.unsubscribeLocked(room, streamID, sender)
		}

	case signaling.ActionPing:
		// Only a coordinator answers a liveness probe. A node that led the call and then joined
		// another half kept answering its old members' pings, so they never saw the coordinator
		// go quiet and stayed behind at the old epoch.
		if room.IsCoordinator {
			m.sendLocked(room, sender, signaling.ActionPong, nil)
		}

	case signaling.ActionStreamReparent:
		var rp signaling.ReparentPayload
		if !room.IsCoordinator {
			m.rejectLocked(room, env, "not the coordinator", false)
		} else if decodePayload(env.Payload, &rp) != nil {
			m.rejectLocked(room, env, "malformed", true)
		} else {
			m.reparentLocked(room, sender, rp, now)
		}

	case signaling.ActionStreamWeak:
		var wp signaling.StreamWeakPayload
		if !room.IsCoordinator {
			m.rejectLocked(room, env, "not the coordinator", false)
		} else if decodePayload(env.Payload, &wp) != nil || len(wp.Weak) > signaling.MaxFeedbackReports {
			m.rejectLocked(room, env, "malformed", true)
		} else {
			m.weakLocked(room, sender, wp)
		}

	case signaling.ActionReport:
		var rp signaling.ReportPayload
		if !room.IsCoordinator {
			m.rejectLocked(room, env, "not the coordinator", false)
		} else if decodePayload(env.Payload, &rp) != nil {
			m.rejectLocked(room, env, "malformed", true)
		} else {
			m.reportLocked(room, sender, rp, now)
		}

	case signaling.ActionKicked:
		if sender != room.Coordinator {
			m.rejectLocked(room, env, "not from the coordinator", true)
		} else {
			var reason string
			_ = decodePayload(env.Payload, &reason) // the reason is only for the log and the UI
			m.logger.Warn("Removed from call by the room", "call_id", calllog.Call(room.CallID), "reason", reason)
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
			m.rejectLocked(room, env, "malformed or not from its coordinator", true)
			return
		}
		if !m.authorityLocked(room, sender, rs.Voice.Epoch) {
			// A coordinator cut off with nobody left counted its members down on its own, so it
			// comes back at a higher epoch. The half that kept the others is the call: the lone
			// half takes its sync even at the older epoch, as it yields to a live heartbeat.
			if !m.aloneLocked(room) || !m.knownInCallLocked(room, sender) || len(rs.Voice.Profiles) <= len(room.ConnectedPeers) {
				m.rejectLocked(room, env, "stale epoch", false)
				return
			}
		}
		m.logger.Debug("Applying room sync", "coordinator", calllog.Peer(sender), "epoch", rs.Voice.Epoch, "streams", len(rs.Streams))
		// a member not admitted yet takes it even at our epoch: one that yielded a room it led
		// alone took the coordinator's epoch, and would otherwise keep its own manifest forever
		if rs.Voice.Epoch > room.Epoch || rs.Voice.Coordinator != room.Coordinator || room.VoiceManifest == nil || !room.joined {
			m.applyVoiceManifestLocked(room, rs.Voice, now)
		}
		live := make(map[string]bool, len(rs.Streams))
		for _, sm := range rs.Streams {
			live[sm.StreamID] = true
			m.applyStreamManifestLocked(room, sm)
		}
		for _, id := range room.StreamEngine.ActiveStreams() {
			if !live[id] {
				m.logger.Info("Stream missing from room sync, closing it", "stream", id)
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
			m.treeLog.Info("Adopting a voice child whose parent went silent", "child", calllog.Peer(sender))
			room.Dispatcher.Adopt(sender)
			m.ensureConnectedLocked(sender)
		} else {
			m.rejectLocked(room, env, "not a member", false)
		}

	case signaling.ActionNack:
		var np signaling.NackPayload
		key := floodKey{env.Sender, env.MsgID}
		if _, dup := room.seen[key]; dup {
			break // the copy that went the other way (datagram or stream) came first
		}
		room.seen[key] = now
		if decodePayload(env.Payload, &np) != nil {
			m.rejectLocked(room, env, "malformed", true)
		} else if up := room.Dispatcher.ServeNack(sender, np.StreamID, np.Lost); len(up) > 0 {
			m.nackUpstreamLocked(room, np.StreamID, up)
		}

	case signaling.ActionStreamFeedback:
		var fp signaling.StreamFeedbackPayload
		if decodePayload(env.Payload, &fp) != nil {
			m.rejectLocked(room, env, "malformed", true)
		} else {
			m.noteFeedbackLocked(room, sender, fp, now)
		}

	case signaling.ActionKeyframeRequest:
		var streamID string
		if decodePayload(env.Payload, &streamID) != nil {
			m.rejectLocked(room, env, "malformed", true)
		} else {
			m.handleKeyframeRequestLocked(room, sender, streamID, now)
		}

	case signaling.ActionMediaKey:
		var mk signaling.MediaKeyPayload
		if decodePayload(env.Payload, &mk) != nil {
			m.rejectLocked(room, env, "malformed", true)
			return
		}
		m.takeMediaKeyLocked(room, sender, mk, now)

	case signaling.ActionMediaKeyAck:
		var ack signaling.KeyAckPayload
		if decodePayload(env.Payload, &ack) != nil {
			m.rejectLocked(room, env, "malformed", true)
			return
		}
		if _, member := room.ConnectedPeers[sender]; !member {
			m.rejectLocked(room, env, "confirmation from outside the call", false)
			return
		}
		m.handleMediaKeyAckLocked(room, sender, ack.Epoch)

	default:
		// a newer peer may speak actions this build does not know
		m.rejectLocked(room, env, "unknown action", false)
	}
}

func (m *CallManager) sendJoinLocked(room *ActiveRoomState, now time.Time) {
	room.lastJoin = now
	m.sendLocked(room, room.Coordinator, signaling.ActionJoin, m.joinPayloadLocked(room))
}

func (m *CallManager) joinPayloadLocked(room *ActiveRoomState) signaling.JoinPayload {
	return signaling.JoinPayload{Profile: room.MyProfile, Proof: signaling.JoinProof(room.RoomKey, room.CallID, m.selfID), KX: m.kxPublicLocked(room), SK: m.signPublicLocked(room)}
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
		m.reparentLocked(room, m.selfID, data.(signaling.ReparentPayload), m.now())
	case signaling.ActionReport:
		m.reportLocked(room, m.selfID, data.(signaling.ReportPayload), m.now())
	case signaling.ActionStreamWeak:
		m.weakLocked(room, m.selfID, data.(signaling.StreamWeakPayload))
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
		m.warnOnce("encode:"+string(action), "Failed to encode call signal", "action", action, "err", err)
		return
	}
	m.sendRawLocked(room, to, action, b)
}

func (m *CallManager) floodLocked(room *ActiveRoomState, action signaling.SignalAction, data any) {
	b, err := m.encodeLocked(room, action, data)
	if err != nil {
		m.warnOnce("encode:"+string(action), "Failed to encode call signal", "action", action, "err", err)
		return
	}
	for _, n := range room.Dispatcher.VoiceNeighbors() {
		m.sendRawLocked(room, n, action, b)
	}
}

// byDatagramToo reports whether a signal goes by datagram as well as by stream: the ones that say
// their sender is alive, and the media key. A mobile link's stream can stall for many seconds
// behind a burst of loss while datagrams still pass (the sound kept flowing): a member that heard
// no heartbeat for two seconds took the call over from a coordinator that was alive, splitting the
// call in two, and a new media key stuck behind the stall silenced whoever waited for it. All of
// them are small, and taking one twice changes nothing.
func byDatagramToo(a signaling.SignalAction) bool {
	switch a {
	case signaling.ActionCoordinatorHeartbeat, signaling.ActionMemberHeartbeat, signaling.ActionPing, signaling.ActionPong,
		signaling.ActionMediaKey:
		return true
	}
	return false
}

// sendRawLocked sends an encoded signal to a peer: sealed if it can be, by datagram too if
// byDatagramToo says so. The receiver takes the first copy (flooded signals by MsgID; the rest are
// idempotent).
func (m *CallManager) sendRawLocked(room *ActiveRoomState, to types.PeerID, action signaling.SignalAction, b []byte) {
	b = m.sealSignalLocked(room, to, b)
	ob := room.Dispatcher.Outbox()
	if byDatagramToo(action) {
		ob.Send(to, media.ClassAudio, transport.SignalLossy, b)
	}
	ob.Send(to, media.ClassControl, transport.Signal, b) // the outbox logs drops
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
