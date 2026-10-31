package call

import (
	"context"
	"log/slog"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// ReportMember flags a member for spam. Reporters are moved off the member's relays at once; the
// member is removed and banned when enough distinct members report it.
func (m *CallManager) ReportMember(peer types.PeerID, reason string) error {
	if peer == m.selfID {
		return ErrSelfReport
	}
	m.mu.Lock()
	room := m.activeRoom
	if room == nil {
		m.mu.Unlock()
		return ErrNotInCall
	}
	if len(reason) > 200 {
		reason = reason[:200]
	}
	m.logger.Info("Reporting call member", "peer", calllog.Peer(peer))
	m.toCoordinatorLocked(room, signaling.ActionReport, signaling.ReportPayload{Target: peer, Reason: reason})
	m.unlockAndFlush()
	return nil
}

// Snapshot returns the current room state.
func (m *CallManager) Snapshot() (RoomSnapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	room := m.activeRoom
	if room == nil {
		return RoomSnapshot{}, false
	}
	s := RoomSnapshot{
		CallID:        room.CallID,
		Coordinator:   room.Coordinator,
		IsCoordinator: room.IsCoordinator,
		Epoch:         room.Epoch,
		Joined:        room.joined,
		Streams:       room.StreamEngine.ActiveStreams(),
	}
	for p := range room.ConnectedPeers {
		s.Members = append(s.Members, p)
	}
	return s, true
}

func (m *CallManager) rebuildVoiceLocked(room *ActiveRoomState) {
	profiles := make([]tree.PeerNodeProfile, 0, len(room.ConnectedPeers))
	for id, p := range room.ConnectedPeers {
		// a member we reach only through a relay most likely has no direct link to the others
		// either: it gets voice from us, never from a member that would first have to find a relay
		if m.relayedLocked(id) {
			p.RelayOnly = true
		}
		profiles = append(profiles, p)
	}
	room.Epoch++
	manifest := tree.BuildVoiceTopology(room.CallID, room.Epoch, m.selfID, profiles)
	m.treeLog.Info("Rebuilt voice tree", "call_id", calllog.Call(room.CallID), "epoch", room.Epoch,
		"members", len(profiles), "coordinator_children", len(manifest.ChildrenMap[m.selfID]))
	m.applyVoiceManifestLocked(room, manifest, time.Now())
	m.floodLocked(room, signaling.ActionTreeManifest, manifest)
}

// aloneLocked: this node is the coordinator of a room nobody else is in.
func (m *CallManager) aloneLocked(room *ActiveRoomState) bool {
	if !room.IsCoordinator {
		return false
	}
	for p := range room.ConnectedPeers {
		if p != m.selfID {
			return false
		}
	}
	return true
}

// yieldLocked gives up a room this node coordinates alone for the room of leader, which still
// has the others: the two halves of a call split by a cut line become one again. The epoch goes
// back to leader's, or its heartbeats and manifests would count as stale.
func (m *CallManager) yieldLocked(room *ActiveRoomState, leader types.PeerID, epoch uint64, now time.Time) {
	m.logger.Info("Back to the coordinator that kept the call going", "call_id", calllog.Call(room.CallID),
		"coordinator", calllog.Peer(leader), "epoch", epoch, "our_epoch", room.Epoch)
	room.Epoch = epoch
	room.formerCoordinator = types.PeerID{}
	room.joined = false
	room.waitingSince = now
	m.setCoordinatorLocked(room, leader, epoch, now)
	m.sendJoinLocked(room, now)
}

// becomeCoordinatorLocked takes over the room after an election this node won.
func (m *CallManager) becomeCoordinatorLocked(room *ActiveRoomState, now time.Time) {
	old := room.Coordinator
	room.Coordinator, room.IsCoordinator = m.selfID, true
	room.candidate = types.PeerID{}
	room.Epoch++
	if old != m.selfID {
		room.formerCoordinator = old
	}

	room.memberSeen = make(map[types.PeerID]time.Time, len(room.ConnectedPeers))
	for p := range room.ConnectedPeers {
		if room.suspects[p] || p == old {
			delete(room.ConnectedPeers, p)
			continue
		}
		room.memberSeen[p] = now
	}
	room.ConnectedPeers[m.selfID] = room.MyProfile
	room.memberSeen[m.selfID] = now

	m.logger.Warn("Took over as call coordinator", "call_id", calllog.Call(room.CallID), "previous", calllog.Peer(old),
		"epoch", room.Epoch, "members", len(room.ConnectedPeers))
	m.floodLocked(room, signaling.ActionCoordinatorElection, m.selfID)
	m.rebuildVoiceLocked(room)

	for _, sm := range room.StreamEngine.Snapshot() {
		if _, alive := room.ConnectedPeers[sm.Broadcaster]; !alive {
			m.logger.Info("Closing stream whose broadcaster left with the old coordinator", "stream", sm.StreamID, "broadcaster", calllog.Peer(sm.Broadcaster))
			m.floodLocked(room, signaling.ActionStreamClose, sm.StreamID)
			m.closeStreamLocalLocked(room, sm.StreamID)
			continue
		}
		for _, sub := range sm.Subscribers {
			if _, alive := room.ConnectedPeers[sub]; !alive {
				_, _ = room.StreamEngine.Unsubscribe(sm.StreamID, sub) // the stream exists, so this cannot fail
			}
		}
		if rebuilt, err := room.StreamEngine.Rebuild(sm.StreamID); err == nil {
			m.floodLocked(room, signaling.ActionStreamTreeUpdate, rebuilt)
		} else {
			m.treeLog.Warn("Could not rebuild stream tree after failover", "stream", sm.StreamID, "err", err)
		}
	}
	// Members cut off from the old tree learn the new state directly.
	for p := range room.ConnectedPeers {
		if p != m.selfID {
			m.sendRoomSyncLocked(room, p)
		}
	}

	m.emit("call:failover_occurred", map[string]any{"call_id": room.CallID, "new_coordinator": m.selfID.String(), "epoch": room.Epoch})
	if cb := m.OnCoordinatorChanged; cb != nil {
		self, epoch := m.selfID, room.Epoch
		m.later(func() { cb(self, epoch) })
	}
}

// ---------------------------------------------------------------------------------------------
// Member role

func (m *CallManager) setCoordinatorLocked(room *ActiveRoomState, leader types.PeerID, epoch uint64, now time.Time) {
	changed := leader != room.Coordinator
	room.Coordinator = leader
	room.IsCoordinator = leader == m.selfID
	if epoch > room.Epoch {
		room.Epoch = epoch
	}
	room.LastCoordPulse = now
	room.candidate = types.PeerID{}
	room.suspects = make(map[types.PeerID]bool)
	if !changed {
		return
	}
	m.logger.Info("Call coordinator changed", "call_id", calllog.Call(room.CallID), "coordinator", calllog.Peer(leader), "epoch", epoch)
	m.ensureConnectedLocked(leader)
	room.lastMemberHB = time.Time{} // introduce ourselves to the new coordinator on the next tick
	m.emit("call:failover_occurred", map[string]any{"call_id": room.CallID, "new_coordinator": leader.String(), "epoch": epoch})
	if cb := m.OnCoordinatorChanged; cb != nil {
		m.later(func() { cb(leader, epoch) })
	}
}

func (m *CallManager) applyVoiceManifestLocked(room *ActiveRoomState, manifest *tree.TreeManifest, now time.Time) {
	if manifest.Coordinator != room.Coordinator {
		m.setCoordinatorLocked(room, manifest.Coordinator, manifest.Epoch, now)
	}
	if old := room.VoiceManifest; old != nil && room.Bridge != nil {
		br := room.Bridge
		for _, p := range old.Members() {
			if _, still := manifest.Profiles[p]; !still && p != manifest.Coordinator {
				peer := p.String()
				m.later(func() { br.ReleaseAudio(peer) })
			}
		}
	}
	var oldAss tree.ParentAssignment
	if room.VoiceManifest != nil {
		oldAss = room.VoiceManifest.ParentMap[m.selfID]
	}
	room.VoiceManifest = manifest
	room.Epoch = manifest.Epoch
	room.IsCoordinator = manifest.Coordinator == m.selfID
	room.LastCoordPulse, room.LastTreePulse = now, now
	room.usingStandby = false
	room.lastParentLost = time.Time{}

	if !room.IsCoordinator {
		room.ConnectedPeers = make(map[types.PeerID]tree.PeerNodeProfile, len(manifest.Profiles))
		for id, p := range manifest.Profiles {
			room.ConnectedPeers[id] = p
		}
		ass, inTree := manifest.ParentMap[m.selfID]
		switch {
		case inTree && !room.joined:
			m.logger.Info("Admitted to the call", "call_id", calllog.Call(room.CallID), "epoch", manifest.Epoch,
				"members", len(manifest.Profiles), "parent", calllog.Peer(ass.PrimaryParent), "standby", calllog.Peer(ass.StandbyParent),
				"waited", now.Sub(room.waitingSince).Round(time.Millisecond).String())
		case !inTree && room.joined:
			room.waitingSince = now
			m.treeLog.Warn("Left out of the new voice tree, asking to join again", "epoch", manifest.Epoch)
		case inTree && ass != oldAss:
			m.treeLog.Info("Voice parent changed", "epoch", manifest.Epoch, "parent", calllog.Peer(ass.PrimaryParent),
				"standby", calllog.Peer(ass.StandbyParent), "previous", calllog.Peer(oldAss.PrimaryParent))
		}
		room.joined = inTree
		room.everJoined = room.everJoined || inTree
	}
	room.Dispatcher.SetVoiceManifest(manifest)

	if ass, ok := manifest.ParentMap[m.selfID]; ok {
		m.ensureConnectedLocked(ass.PrimaryParent)
		m.ensureConnectedLocked(ass.StandbyParent)
	}
	for _, c := range manifest.ChildrenMap[m.selfID] {
		m.ensureConnectedLocked(c)
	}
	if cb := m.OnTopologyUpdated; cb != nil {
		m.later(func() { cb(manifest) })
	}
}

func (m *CallManager) upstreamLocked(room *ActiveRoomState) types.PeerID {
	if room.VoiceManifest == nil {
		return types.PeerID{}
	}
	ass := room.VoiceManifest.ParentMap[m.selfID]
	if room.usingStandby {
		return ass.StandbyParent
	}
	return ass.PrimaryParent
}

// reparentLocked (coordinator) moves sub away from a relay. A "silent" report also starts the
// fast liveness check of that relay, exactly like a voice ParentLost.
func (m *CallManager) reparentLocked(room *ActiveRoomState, sub types.PeerID, rp signaling.ReparentPayload, now time.Time) {
	if rp.Reason == "silent" && rp.Avoid != m.selfID {
		// (a report about us is stale, from a member that was cut off: we know we are alive)
		if _, member := room.memberSeen[rp.Avoid]; member {
			if _, already := room.reportedLost[rp.Avoid]; !already {
				room.reportedLost[rp.Avoid] = now
			}
		}
	}
	sm, err := room.StreamEngine.Reparent(rp.StreamID, sub, rp.Avoid)
	if err != nil {
		m.treeLog.Warn("Could not move stream subscriber to another relay", "stream", rp.StreamID,
			"subscriber", calllog.Peer(sub), "avoid", calllog.Peer(rp.Avoid), "reason", rp.Reason, "err", err)
		return
	}
	m.treeLog.Info("Moved stream subscriber to another relay", "stream", rp.StreamID, "subscriber", calllog.Peer(sub),
		"avoid", calllog.Peer(rp.Avoid), "parent", calllog.Peer(sm.ParentMap[sub]), "reason", rp.Reason)
	m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
	m.afterStreamTreeChangeLocked(room, sm)
	m.emit("call:relay_changed", map[string]any{"call_id": room.CallID, "stream": rp.StreamID, "subscriber": sub.String(), "avoid": rp.Avoid.String(), "reason": rp.Reason})
}

// reportLocked (coordinator) records a spam report. The reporter is immediately fed around the
// target; the target is kicked once a third of the other members (at least two) agree.
func (m *CallManager) reportLocked(room *ActiveRoomState, reporter types.PeerID, rp signaling.ReportPayload, now time.Time) {
	target := rp.Target
	if target == reporter || target == m.selfID {
		m.logger.Debug("Ignored report against the reporter or the coordinator", "reporter", calllog.Peer(reporter), "target", calllog.Peer(target))
		return // the coordinator cannot vote itself out; members can still mute it locally
	}
	if _, member := room.ConnectedPeers[target]; !member {
		m.logger.Debug("Ignored report against a non-member", "reporter", calllog.Peer(reporter), "target", calllog.Peer(target))
		return
	}
	if _, member := room.ConnectedPeers[reporter]; !member {
		m.logger.Debug("Ignored report from a non-member", "reporter", calllog.Peer(reporter), "target", calllog.Peer(target))
		return
	}
	for _, sm := range room.StreamEngine.Snapshot() {
		if sm.Broadcaster == target {
			continue
		}
		if moved, err := room.StreamEngine.Reparent(sm.StreamID, reporter, target); err == nil && moved.Epoch != sm.Epoch {
			m.floodLocked(room, signaling.ActionStreamTreeUpdate, moved)
		}
	}

	byTarget := room.reports[target]
	if byTarget == nil {
		byTarget = make(map[types.PeerID]time.Time)
		room.reports[target] = byTarget
	}
	byTarget[reporter] = now
	votes := 0
	for r, at := range byTarget {
		if _, member := room.ConnectedPeers[r]; member && now.Sub(at) < reportWindow {
			votes++
		}
	}
	others := len(room.ConnectedPeers) - 1
	need := max(2, (others+2)/3)
	m.logger.Info("Call member reported", "reporter", calllog.Peer(reporter), "target", calllog.Peer(target), "votes", votes, "needed", need)
	m.emit("call:member_reported", map[string]any{"call_id": room.CallID, "target": target.String(), "votes": votes, "needed": need})
	if votes < need {
		return
	}

	m.logger.Warn("Kicking reported call member", "peer", calllog.Peer(target), "votes", votes)
	room.banned[target] = true
	delete(room.reports, target)
	if kicked, err := m.encodeLocked(room, signaling.ActionKicked, "reported for spam"); err == nil && m.sender != nil {
		sender, log := m.sender, m.logger
		go func() {
			if err := sender.SendRawFrame(target, net_types.TypeCallSignaling, kicked); err != nil {
				log.Debug("Could not tell a kicked member", "peer", calllog.Peer(target), "err", err)
			}
		}()
	}
	m.removeMemberLocked(room, target, "kicked")
	if cb := m.OnMemberKicked; cb != nil {
		m.later(func() { cb(target) })
	}
}

// ---------------------------------------------------------------------------------------------
// Loss recovery: a NACK to the parent first, a keyframe request once the reorder window runs out

func (m *CallManager) sendNack(streamID string, lost []media.NackEntry) {
	m.mu.Lock()
	if room := m.activeRoom; room != nil {
		m.nackUpstreamLocked(room, streamID, lost)
	}
	m.unlockAndFlush()
}

func (m *CallManager) nackUpstreamLocked(room *ActiveRoomState, streamID string, lost []media.NackEntry) {
	if up, ok := room.StreamEngine.GetUpstream(streamID, m.selfID); ok {
		m.sendLocked(room, up, signaling.ActionNack, signaling.NackPayload{StreamID: streamID, Lost: lost})
		return
	}
	m.debugOnce("nack-noup:"+streamID, "Lost video units but no parent to ask on this stream", "stream", streamID, "units", len(lost))
}

// requestKeyframe asks for a keyframe this node needs itself (a loss, or the browser's PLI): not
// again while the last one is still on its way. Requests children pass up are not held back here;
// the per-hop interval of requestKeyframeLocked merges those.
func (m *CallManager) requestKeyframe(streamID string) {
	m.mu.Lock()
	if room := m.activeRoom; room != nil && (room.Dispatcher == nil || room.Dispatcher.WantKeyframe(streamID)) {
		m.requestKeyframeLocked(room, streamID, time.Now())
	}
	m.unlockAndFlush()
}

func (m *CallManager) requestKeyframeLocked(room *ActiveRoomState, streamID string, now time.Time) {
	if now.Sub(room.keyframeSent[streamID]) < keyframeMinInterval {
		return
	}
	up, ok := room.StreamEngine.GetUpstream(streamID, m.selfID)
	if !ok {
		m.debugOnce("kf-noup:"+streamID, "Keyframe needed but no parent on this stream", "stream", streamID)
		return
	}
	room.keyframeSent[streamID] = now
	m.debugOnce("kf:"+streamID, "Asking for a keyframe", "stream", streamID, "parent", calllog.Peer(up))
	m.sendLocked(room, up, signaling.ActionKeyframeRequest, streamID)
}

// handleKeyframeRequestLocked serves the request if we broadcast the stream, otherwise passes
// it one hop up; per-hop rate limiting collapses a burst from many viewers into one PLI.
func (m *CallManager) handleKeyframeRequestLocked(room *ActiveRoomState, streamID string, now time.Time) {
	ann, local := room.localStreams[streamID]
	if !local {
		m.requestKeyframeLocked(room, streamID, now)
		return
	}
	role := captureRole(ann.MediaType)
	if now.Sub(room.keyframeServed[streamID]) < keyframeServeInterval {
		return
	}
	room.keyframeServed[streamID] = now
	m.keyframesServed.Add(1)
	m.debugOnce("kf-serve:"+streamID, "Serving keyframe request for our stream", "stream", streamID, "role", role)
	br, cb := room.Bridge, m.OnKeyframeRequest
	m.later(func() {
		if br != nil {
			_ = br.RequestKeyframe(role) // the bridge logs a failure
		}
		if cb != nil {
			cb(streamID)
		}
	})
}

// ---------------------------------------------------------------------------------------------
// Periodic work

func (m *CallManager) runLoop(ctx context.Context, room *ActiveRoomState) {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.mu.Lock()
			if m.activeRoom != room {
				m.mu.Unlock()
				return
			}
			if room.IsCoordinator {
				m.coordinatorTickLocked(room, now)
			} else {
				m.memberTickLocked(room, now)
			}
			m.rateTickLocked(room, now)
			m.statsTickLocked(room, now)
			if now.Sub(room.lastSeenGC) > time.Second {
				room.lastSeenGC = now
				for k, ts := range room.seen {
					if now.Sub(ts) > seenFloodTTL {
						delete(room.seen, k)
					}
				}
			}
			m.unlockAndFlush()
		}
	}
}

func (m *CallManager) coordinatorTickLocked(room *ActiveRoomState, now time.Time) {
	if f := room.formerCoordinator; f != (types.PeerID{}) && m.aloneLocked(room) && now.Sub(room.lastRejoinProbe) >= joinRetryInterval {
		// alone after taking over: the old coordinator may well be alive with the others, and
		// answering this with its heartbeat brings us back (see yieldLocked)
		room.lastRejoinProbe = now
		m.ensureConnectedLocked(f)
		m.sendLocked(room, f, signaling.ActionJoin, m.joinPayloadLocked(room))
	}
	if now.Sub(room.lastCoordHB) >= coordHeartbeatInterval {
		room.lastCoordHB = now
		hb := signaling.CoordinatorHeartbeatPayload{Coordinator: m.selfID, Epoch: room.Epoch, Streams: make(map[string]uint64)}
		for _, sm := range room.StreamEngine.Snapshot() {
			hb.Streams[sm.StreamID] = sm.Epoch
		}
		m.floodLocked(room, signaling.ActionCoordinatorHeartbeat, hb)
	}
	for p, seen := range room.memberSeen {
		if p != m.selfID && now.Sub(seen) > m.waitLocked(p, memberTimeout, relayMemberTimeout) {
			m.removeMemberLocked(room, p, "timeout")
		}
	}
	// A child reported its parent silent: drop the parent as soon as its heartbeats confirm it,
	// instead of waiting for the full member timeout. A heartbeat after the report means only
	// the link broke, so the parent stays.
	for p, reportedAt := range room.reportedLost {
		seen, member := room.memberSeen[p]
		switch {
		case !member || p == m.selfID || seen.After(reportedAt.Add(memberHeartbeatInterval)):
			delete(room.reportedLost, p)
		case now.Sub(seen) > m.waitLocked(p, parentTimeout, relayParentTimeout):
			delete(room.reportedLost, p)
			m.removeMemberLocked(room, p, "reported silent by a child")
		}
	}
	if now.Sub(room.lastReconcile) >= reconcileInterval {
		room.lastReconcile = now
		m.reconcileLocked(room)
	}
	m.feederWatchdogLocked(room, now)
}

// feederWatchdogLocked asks to be re-homed when the relay feeding a stream goes quiet. Feeders
// pulse idle links every 250 ms, so silence means a dead relay or a broken link.
func (m *CallManager) feederWatchdogLocked(room *ActiveRoomState, now time.Time) {
	for streamID, feeder := range room.StreamEngine.Upstreams(m.selfID) {
		if room.Dispatcher.FeederSilence(streamID, feeder, now) < m.waitLocked(feeder, streamFeederTimeout, relayFeederTimeout) {
			continue
		}
		if now.Sub(room.reparentAsked[streamID]) < reparentRetry {
			continue
		}
		room.reparentAsked[streamID] = now
		m.logger.Warn("Stream feeder silent, asking for another relay", "stream", streamID, "feeder", calllog.Peer(feeder),
			"silent", room.Dispatcher.FeederSilence(streamID, feeder, now).Round(time.Millisecond).String())
		m.toCoordinatorLocked(room, signaling.ActionStreamReparent, signaling.ReparentPayload{StreamID: streamID, Avoid: feeder, Reason: "silent"})
	}
}

func (m *CallManager) memberTickLocked(room *ActiveRoomState, now time.Time) {
	if !room.joined {
		if now.Sub(room.lastJoin) >= joinRetryInterval {
			if waited := now.Sub(room.waitingSince); waited > 3*joinRetryInterval {
				// the coordinator is unreachable, rejects our proof, or has us banned
				m.warnOnce("joinwait", "Still waiting to be admitted to the call", "call_id", calllog.Call(room.CallID),
					"coordinator", calllog.Peer(room.Coordinator), "waited", waited.Round(time.Second).String())
			}
			m.ensureConnectedLocked(room.Coordinator)
			m.sendJoinLocked(room, now)
		}
	} else if now.Sub(room.lastMemberHB) >= memberHeartbeatInterval {
		room.lastMemberHB = now
		m.sendLocked(room, room.Coordinator, signaling.ActionMemberHeartbeat, m.joinPayloadLocked(room))
	}

	// Parent watchdog: no pulse from upstream means the parent (or the whole room) is gone. It
	// keeps running on the standby parent, so a standby that dies too is caught as well.
	if room.joined && room.VoiceManifest != nil &&
		now.Sub(room.LastTreePulse) > m.waitLocked(m.upstreamLocked(room), parentTimeout, relayParentTimeout) {
		m.parentWatchdogLocked(room, now)
	}

	// Coordinator watchdog: probe directly before declaring it dead.
	silent := now.Sub(room.LastCoordPulse)
	coordWait := m.waitLocked(room.Coordinator, coordinatorTimeout, relayCoordinatorTimeout)
	if silent > coordWait/2 && now.Sub(room.lastPing) >= pingInterval {
		room.lastPing = now
		m.debugOnce("ping", "Coordinator quiet, probing it", "coordinator", calllog.Peer(room.Coordinator), "silent", silent.Round(time.Millisecond).String())
		m.sendLocked(room, room.Coordinator, signaling.ActionPing, nil)
	}
	if room.everJoined && silent > coordWait {
		m.electLocked(room, now)
	}

	if room.joined && now.Sub(room.lastReconcile) >= reconcileInterval {
		room.lastReconcile = now
		m.reconcileLocked(room)
	}
	if room.joined {
		m.feederWatchdogLocked(room, now)
	}
}

// parentWatchdogLocked fails over to the standby parent once, then keeps telling the coordinator
// which parent went silent until a new manifest re-homes us. The report is repeated at most every
// parentLostRetry, not on every tick.
func (m *CallManager) parentWatchdogLocked(room *ActiveRoomState, now time.Time) {
	ass := room.VoiceManifest.ParentMap[m.selfID]
	silent := now.Sub(room.LastTreePulse).Round(time.Millisecond).String()
	if !room.usingStandby && ass.StandbyParent != (types.PeerID{}) && ass.StandbyParent != ass.PrimaryParent {
		room.usingStandby = true
		room.LastTreePulse = now
		room.lastParentLost = now
		room.Dispatcher.UseStandbyParent(ass.StandbyParent)
		m.ensureConnectedLocked(ass.StandbyParent)
		m.sendLocked(room, ass.StandbyParent, signaling.ActionAdopt, nil)
		m.treeLog.Warn("Voice parent silent, switched to standby", "parent", calllog.Peer(ass.PrimaryParent), "standby", calllog.Peer(ass.StandbyParent))
		m.sendLocked(room, room.Coordinator, signaling.ActionParentLost, ass.PrimaryParent)
		return
	}
	if now.Sub(room.lastParentLost) < parentLostRetry {
		return
	}
	room.lastParentLost = now
	// nothing (left) to fail over to: voice is cut until the coordinator rebuilds the tree
	if room.usingStandby {
		m.lim.Log(m.treeLog, slog.LevelWarn, "standbylost", "Standby voice parent silent too",
			"parent", calllog.Peer(ass.PrimaryParent), "standby", calllog.Peer(ass.StandbyParent), "silent", silent)
		m.sendLocked(room, room.Coordinator, signaling.ActionParentLost, ass.StandbyParent)
	} else {
		m.lim.Log(m.treeLog, slog.LevelWarn, "parentlost", "Voice parent silent and no standby parent",
			"parent", calllog.Peer(ass.PrimaryParent), "silent", silent)
	}
	m.sendLocked(room, room.Coordinator, signaling.ActionParentLost, ass.PrimaryParent)
}

// electLocked runs the deterministic election. Only the winner announces itself; others wait,
// and if the winner stays silent it is suspected too and the next candidate is tried.
func (m *CallManager) electLocked(room *ActiveRoomState, now time.Time) {
	if room.candidate != (types.PeerID{}) {
		if now.Before(room.candidateDeadline) {
			return
		}
		m.logger.Warn("Election candidate stayed silent, trying the next one", "candidate", calllog.Peer(room.candidate))
		room.suspects[room.candidate] = true
	} else {
		m.logger.Warn("Call coordinator silent, electing a new one", "coordinator", calllog.Peer(room.Coordinator),
			"silent", now.Sub(room.LastCoordPulse).Round(time.Millisecond).String())
	}
	room.suspects[room.Coordinator] = true

	alive := map[types.PeerID]bool{m.selfID: true}
	for p := range room.ConnectedPeers {
		if !room.suspects[p] {
			alive[p] = true
		}
	}
	var standbys []types.PeerID
	if room.VoiceManifest != nil {
		standbys = room.VoiceManifest.StandbyCoordinators
	}
	leader, ok := tree.ElectNewCoordinator(standbys, alive)
	if !ok {
		m.warnOnce("noleader", "No coordinator candidate left, the call is stalled", "standbys", len(standbys), "alive", len(alive))
		return
	}
	if leader == m.selfID {
		m.becomeCoordinatorLocked(room, now)
		return
	}
	m.logger.Info("Waiting for the elected coordinator", "candidate", calllog.Peer(leader))
	room.candidate, room.candidateDeadline = leader, now.Add(m.waitLocked(leader, electionWait, relayElectionWait))
	m.ensureConnectedLocked(leader)
	m.sendLocked(room, leader, signaling.ActionJoin, m.joinPayloadLocked(room))
}

// replicaStaleLocked compares the coordinator's digest with our replicas.
func (m *CallManager) replicaStaleLocked(room *ActiveRoomState, hb signaling.CoordinatorHeartbeatPayload) bool {
	if hb.Epoch > room.Epoch {
		return true
	}
	local := room.StreamEngine.ActiveStreams()
	if len(local) != len(hb.Streams) {
		return true
	}
	for _, id := range local {
		sm, ok := room.StreamEngine.GetStreamManifest(id)
		if !ok || hb.Streams[id] != sm.Epoch {
			return true
		}
	}
	return false
}

// reconcileLocked repairs lost or reordered requests (a subscribe may overtake the stream's
// registration): re-register our streams and re-subscribe to watched ones.
func (m *CallManager) reconcileLocked(room *ActiveRoomState) {
	for id, ann := range room.localStreams {
		if _, ok := room.StreamEngine.GetStreamManifest(id); !ok {
			ann.Profile = room.MyProfile
			m.toCoordinatorLocked(room, signaling.ActionStreamRegister, ann)
		}
	}
	for id := range room.watching {
		if _, ok := room.StreamEngine.GetStreamManifest(id); !ok {
			continue
		}
		if _, attached := room.StreamEngine.GetUpstream(id, m.selfID); !attached {
			m.toCoordinatorLocked(room, signaling.ActionStreamSubscribe, signaling.StreamSubscribePayload{
				StreamID: id, Subscriber: m.selfID, Profile: room.MyProfile,
			})
		}
	}
}
