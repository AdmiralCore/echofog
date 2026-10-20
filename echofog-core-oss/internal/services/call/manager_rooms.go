package call

import (
	"context"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// JoinRoom enters a group call with a specified coordinator and room E2EE key.
func (m *CallManager) JoinRoom(callID string, coordinator types.PeerID, roomKey []byte, profile tree.PeerNodeProfile) error {
	if len(roomKey) != 32 {
		m.logger.Warn("Call join refused: bad room key", "call_id", calllog.Call(callID), "key_len", len(roomKey))
		return ErrInvalidRoomKey
	}

	m.mu.Lock()
	if m.activeRoom != nil {
		active := m.activeRoom.CallID
		m.mu.Unlock()
		m.logger.Warn("Call join refused: already in a call", "call_id", calllog.Call(callID), "active_call", calllog.Call(active))
		return ErrCallAlreadyActive
	}

	ctx, cancel := context.WithCancel(context.Background())
	engine := tree.NewStreamTreeEngine()
	profile.PeerID = m.selfID
	now := time.Now()

	room := &ActiveRoomState{
		CallID:         callID,
		Coordinator:    coordinator,
		IsCoordinator:  coordinator == m.selfID,
		RoomKey:        append([]byte(nil), roomKey...),
		StreamEngine:   engine,
		LastCoordPulse: now,
		LastTreePulse:  now,
		ConnectedPeers: make(map[types.PeerID]tree.PeerNodeProfile),
		MyProfile:      profile,
		Cancel:         cancel,
		memberSeen:     make(map[types.PeerID]time.Time),
		reportedLost:   make(map[types.PeerID]time.Time),
		reports:        make(map[types.PeerID]map[types.PeerID]time.Time),
		banned:         make(map[types.PeerID]bool),
		reparentAsked:  make(map[string]time.Time),
		suspects:       make(map[types.PeerID]bool),
		localStreams:   make(map[string]signaling.StreamAnnouncePayload),
		localByRole:    make(map[string]string),
		watching:       make(map[string]bool),
		announces:      make(map[string]signaling.StreamAnnouncePayload),
		seen:           make(map[floodKey]time.Time),
		keyframeSent:   make(map[string]time.Time),
		keyframeServed: make(map[string]time.Time),
		startedAt:      now,
		waitingSince:   now,
		lastStatsLog:   now,
	}

	room.Dispatcher = media.NewMediaDispatcher(m.selfID, m.sender, m, engine)
	if err := room.Dispatcher.SetRoomKey(roomKey); err != nil {
		cancel()
		room.Dispatcher.Close()
		m.mu.Unlock()
		m.logger.Warn("Call join failed: room key rejected", "call_id", calllog.Call(callID), "err", err)
		return err
	}
	room.Dispatcher.OnVideoNack = m.sendNack
	room.Dispatcher.OnVideoLoss = func(streamID string, _ types.PeerID) { m.requestKeyframe(streamID) }

	if b, err := bridge.NewLocalMediaBridge(bridge.Options{
		OnLocalRTP:       m.onBridgeRTP,
		OnSlot:           m.onBridgeSlot,
		OnKeyframeNeeded: m.requestKeyframe,
	}); err == nil {
		room.Bridge = b
	} else {
		// the call still runs (and relays) without it, but the UI gets no audio or video
		m.logger.Warn("Media bridge could not be created, the call has no local audio or video", "call_id", calllog.Call(callID), "err", err)
	}

	m.activeRoom = room
	if room.IsCoordinator {
		room.ConnectedPeers[m.selfID] = profile
		room.memberSeen[m.selfID] = now
		room.joined, room.everJoined = true, true
		m.rebuildVoiceLocked(room)
	} else {
		m.ensureConnectedLocked(coordinator)
		m.sendJoinLocked(room, now)
	}

	m.logger.Info("Joined call room", "call_id", calllog.Call(callID), "coordinator", calllog.Peer(coordinator), "is_coord", room.IsCoordinator)
	m.emit("call:session_started", map[string]any{"call_id": callID, "coordinator": coordinator.String(), "is_coordinator": room.IsCoordinator})
	m.unlockAndFlush()

	go m.runLoop(ctx, room)
	return nil
}

// LeaveRoom exits the current call and cleans up resources.
func (m *CallManager) LeaveRoom() error {
	m.mu.Lock()
	room := m.activeRoom
	if room == nil {
		m.mu.Unlock()
		return nil
	}

	// Tell the room directly; the outbox dies with the room, so bypass it.
	if m.sender != nil {
		var targets []types.PeerID
		if room.IsCoordinator {
			for p := range room.ConnectedPeers {
				if p != m.selfID {
					targets = append(targets, p)
				}
			}
		} else {
			targets = []types.PeerID{room.Coordinator}
		}
		if leave, err := m.encodeLocked(room, signaling.ActionLeave, nil); err == nil {
			sender, log := m.sender, m.logger
			for _, t := range targets {
				go func(t types.PeerID) {
					if err := sender.SendRawFrame(t, net_types.TypeCallSignaling, leave); err != nil {
						// the peer finds out from the missing heartbeats, only later
						log.Debug("Could not tell a call peer that we left", "peer", calllog.Peer(t), "err", err)
					}
				}(t)
			}
		} else {
			m.logger.Warn("Failed to encode call leave signal", "err", err)
		}
	}

	m.logSummaryLocked(room)
	room.Cancel()
	room.Dispatcher.Close()
	if room.Bridge != nil {
		_ = room.Bridge.Close() // the bridge logs its own close failure
	}
	m.activeRoom = nil

	m.logger.Info("Left call room", "call_id", calllog.Call(room.CallID), "was_coord", room.IsCoordinator,
		"duration", time.Since(room.startedAt).Round(time.Second).String())
	m.emit("call:session_ended", map[string]any{"call_id": room.CallID})
	m.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------------------------
// Coordinator role

func (m *CallManager) admitLocked(room *ActiveRoomState, peer types.PeerID, profile tree.PeerNodeProfile, now time.Time) {
	profile.PeerID = peer
	_, known := room.ConnectedPeers[peer]
	room.ConnectedPeers[peer] = profile
	room.memberSeen[peer] = now
	delete(room.suspects, peer)
	room.StreamEngine.UpdatePeerProfile(profile)
	m.ensureConnectedLocked(peer)
	if !known {
		m.logger.Info("Call member joined", "call_id", calllog.Call(room.CallID), "peer", calllog.Peer(peer),
			"members", len(room.ConnectedPeers), "upload_kbps", profile.UploadKbps, "relay_children", profile.MaxRelayChildren)
		m.rebuildVoiceLocked(room)
	}
	m.sendRoomSyncLocked(room, peer)
}

func (m *CallManager) sendRoomSyncLocked(room *ActiveRoomState, peer types.PeerID) {
	rs := signaling.RoomSyncPayload{Voice: room.VoiceManifest, Streams: room.StreamEngine.Snapshot()}
	for _, ann := range room.announces {
		rs.Announces = append(rs.Announces, ann)
	}
	m.sendLocked(room, peer, signaling.ActionRoomSync, rs)
}

// removeMemberLocked drops a member; reason says why ("left", "timeout", "parent lost", "kicked").
func (m *CallManager) removeMemberLocked(room *ActiveRoomState, peer types.PeerID, reason string) {
	delete(room.ConnectedPeers, peer)
	delete(room.memberSeen, peer)
	if reason == "left" {
		m.logger.Info("Call member left", "call_id", calllog.Call(room.CallID), "peer", calllog.Peer(peer), "members", len(room.ConnectedPeers))
	} else {
		m.logger.Warn("Removing call member", "call_id", calllog.Call(room.CallID), "peer", calllog.Peer(peer), "reason", reason, "members", len(room.ConnectedPeers))
	}
	m.emit("call:member_removed", map[string]any{"call_id": room.CallID, "peer": peer.String()})

	m.rebuildVoiceLocked(room)
	changed, closed := room.StreamEngine.RemovePeer(peer)
	for _, sm := range changed {
		m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
	}
	for _, id := range closed {
		m.logger.Info("Closing stream of a member that is gone", "stream", id, "broadcaster", calllog.Peer(peer))
		m.floodLocked(room, signaling.ActionStreamClose, id)
		m.closeStreamLocalLocked(room, id)
	}
	if ob := room.Dispatcher.Outbox(); ob != nil {
		ob.Forget(peer)
	}
}

// ensureConnectedLocked dials a tree neighbour in the background if we have no link to it.
func (m *CallManager) ensureConnectedLocked(peer types.PeerID) {
	if m.connector == nil || peer == m.selfID || peer == (types.PeerID{}) {
		return
	}
	if m.swarm != nil && m.swarm.GetPeer(peer) != nil {
		return
	}
	now := time.Now()
	if now.Sub(m.connecting[peer]) < connectRetryInterval {
		return
	}
	m.connecting[peer] = now
	connect := m.connector
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := connect(ctx, peer); err != nil {
			// a tree neighbour we cannot reach gets no media from us and sends us none
			m.warnOnce("connect:"+peer.String(), "Could not reach call peer", "peer", calllog.Peer(peer), "err", err)
			return
		}
		m.logger.Debug("Connected to call peer", "peer", calllog.Peer(peer))
	}()
}
