package call

import (
	"errors"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// StartScreenShare announces a stream using only a bitrate target (legacy entry point).
func (m *CallManager) StartScreenShare(streamID string, mediaType string, bitrateKbps int) error {
	return m.StartStream(streamID, mediaType, tree.CustomQuality(bitrateKbps))
}

// StartStream announces and broadcasts a local screen or camera stream at the given quality.
func (m *CallManager) StartStream(streamID string, mediaType string, q tree.StreamQuality) error {
	err := m.startStream(streamID, mediaType, q)
	if err != nil {
		m.logger.Warn("Stream start failed", "stream", streamID, "media", mediaType, "quality", q.ID, "kbps", q.BitrateKbps, "err", err)
	}
	return err
}

func (m *CallManager) startStream(streamID string, mediaType string, q tree.StreamQuality) error {
	if len(streamID) == 0 || len(streamID) > media.MaxStreamIDLen || streamID == "voice" {
		return fmt.Errorf("invalid stream id %q", streamID)
	}
	m.mu.Lock()
	room := m.activeRoom
	if room == nil {
		m.mu.Unlock()
		return ErrNotInCall
	}
	if !q.FitsUpload(room.MyProfile.UploadKbps) {
		upload := room.MyProfile.UploadKbps
		m.mu.Unlock()
		return fmt.Errorf("%w: %s needs %d kbps, available %d kbps", tree.ErrInsufficientUpload, q.Label, q.BitrateKbps, upload)
	}
	if prev, ok := room.announces[streamID]; ok && prev.Broadcaster != m.selfID {
		m.mu.Unlock()
		return tree.ErrStreamAlreadyExist
	}

	m.logger.Info("Stream started", "call_id", calllog.Call(room.CallID), "stream", streamID, "media", mediaType,
		"quality", q.ID, "kbps", q.BitrateKbps, "width", q.Width, "height", q.Height, "fps", q.FPS, "upload_kbps", room.MyProfile.UploadKbps)
	ann := m.announceFor(room, streamID, mediaType, q)
	room.localStreams[streamID] = ann
	room.localByRole[captureRole(mediaType)] = streamID
	m.toCoordinatorLocked(room, signaling.ActionStreamRegister, ann)
	if !room.IsCoordinator {
		m.notifyAnnounceLocked(ann) // the coordinator path already notified
	}
	m.unlockAndFlush()
	return nil
}

// UpdateStreamQuality switches an active stream to another quality and re-announces it to the room.
func (m *CallManager) UpdateStreamQuality(streamID string, q tree.StreamQuality) error {
	err := m.updateStreamQuality(streamID, q)
	if err != nil {
		m.logger.Warn("Stream quality change failed", "stream", streamID, "quality", q.ID, "kbps", q.BitrateKbps, "err", err)
	} else {
		m.logger.Info("Stream quality changed", "stream", streamID, "quality", q.ID, "kbps", q.BitrateKbps, "width", q.Width, "height", q.Height, "fps", q.FPS)
	}
	return err
}

func (m *CallManager) updateStreamQuality(streamID string, q tree.StreamQuality) error {
	m.mu.Lock()
	room := m.activeRoom
	if room == nil {
		m.mu.Unlock()
		return ErrNotInCall
	}
	prev, ok := room.localStreams[streamID]
	if !ok {
		m.mu.Unlock()
		return ErrNoLocalStream
	}
	if !q.FitsUpload(room.MyProfile.UploadKbps) {
		upload := room.MyProfile.UploadKbps
		m.mu.Unlock()
		return fmt.Errorf("%w: %s needs %d kbps, available %d kbps", tree.ErrInsufficientUpload, q.Label, q.BitrateKbps, upload)
	}
	ann := m.announceFor(room, streamID, prev.MediaType, q)
	room.localStreams[streamID] = ann
	m.toCoordinatorLocked(room, signaling.ActionStreamRegister, ann)
	if !room.IsCoordinator {
		m.notifyAnnounceLocked(ann)
	}
	m.unlockAndFlush()
	return nil
}

func (m *CallManager) announceFor(room *ActiveRoomState, streamID, mediaType string, q tree.StreamQuality) signaling.StreamAnnouncePayload {
	return signaling.StreamAnnouncePayload{
		StreamID:    streamID,
		Broadcaster: m.selfID,
		MediaType:   mediaType,
		BitrateKbps: q.BitrateKbps,
		Width:       q.Width,
		Height:      q.Height,
		FPS:         q.FPS,
		Codec:       q.Codec,
		QualityID:   q.ID,
		Profile:     room.MyProfile,
	}
}

// SetUploadCapacity updates the measured or user-declared upstream bandwidth used to validate stream quality.
func (m *CallManager) SetUploadCapacity(kbps int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeRoom != nil {
		m.activeRoom.MyProfile.UploadKbps = kbps
	}
}

// StopScreenShare closes the broadcast.
func (m *CallManager) StopScreenShare(streamID string) error {
	m.mu.Lock()
	room := m.activeRoom
	if room == nil {
		m.mu.Unlock()
		m.logger.Warn("Stream stop failed", "stream", streamID, "err", ErrNotInCall)
		return ErrNotInCall
	}
	if _, ok := room.localStreams[streamID]; ok {
		m.logger.Info("Stream stopped", "stream", streamID)
	} else {
		m.logger.Debug("Stream stop for a stream we do not broadcast", "stream", streamID)
	}
	delete(room.localStreams, streamID)
	for role, id := range room.localByRole {
		if id == streamID {
			delete(room.localByRole, role)
		}
	}
	// OnStreamClosed fires when the coordinator's close flood comes back (or in place if we are it)
	m.toCoordinatorLocked(room, signaling.ActionStreamUnregister, streamID)
	m.unlockAndFlush()
	return nil
}

// WatchStream subscribes to another peer's stream (Selective Subscription).
func (m *CallManager) WatchStream(streamID string) error {
	m.mu.Lock()
	room := m.activeRoom
	if room == nil {
		m.mu.Unlock()
		m.logger.Warn("Watching a stream failed", "stream", streamID, "err", ErrNotInCall)
		return ErrNotInCall
	}
	m.logger.Info("Watching stream", "stream", streamID, "known", room.announces[streamID].StreamID != "")
	room.watching[streamID] = true
	room.Dispatcher.SetStreamWatching(streamID, true)
	m.toCoordinatorLocked(room, signaling.ActionStreamSubscribe, signaling.StreamSubscribePayload{
		StreamID: streamID, Subscriber: m.selfID, Profile: room.MyProfile,
	})
	m.unlockAndFlush()
	return nil
}

// UnwatchStream cancels watching a stream.
func (m *CallManager) UnwatchStream(streamID string) error {
	m.mu.Lock()
	room := m.activeRoom
	if room == nil {
		m.mu.Unlock()
		return ErrNotInCall
	}
	m.logger.Info("Stopped watching stream", "stream", streamID)
	delete(room.watching, streamID)
	room.Dispatcher.SetStreamWatching(streamID, false)
	m.releaseVideoSlotLocked(room, streamID)
	m.toCoordinatorLocked(room, signaling.ActionStreamUnsub, streamID)
	m.unlockAndFlush()
	return nil
}

// RequestRelayChange asks the coordinator to feed a watched stream through another relay, e.g.
// when the current one lags or injects garbage.
func (m *CallManager) RequestRelayChange(streamID string) error {
	m.mu.Lock()
	room := m.activeRoom
	if room == nil {
		m.mu.Unlock()
		return ErrNotInCall
	}
	parent, ok := room.StreamEngine.GetUpstream(streamID, m.selfID)
	if !ok {
		m.mu.Unlock()
		m.logger.Warn("Relay change refused: not attached to the stream", "stream", streamID)
		return ErrNotWatching
	}
	m.logger.Info("Asking for another relay on user request", "stream", streamID, "relay", calllog.Peer(parent))
	room.reparentAsked[streamID] = time.Now()
	m.toCoordinatorLocked(room, signaling.ActionStreamReparent, signaling.ReparentPayload{StreamID: streamID, Avoid: parent, Reason: "user"})
	m.unlockAndFlush()
	return nil
}

// releaseVideoSlotLocked frees the playback track after the lock is dropped: the bridge reports
// the release through OnMediaSlot, which may call back into the manager.
func (m *CallManager) releaseVideoSlotLocked(room *ActiveRoomState, streamID string) {
	if br := room.Bridge; br != nil {
		m.later(func() { br.ReleaseVideo(streamID) })
	}
}

// StreamTree returns this node's replica of a stream distribution tree.
func (m *CallManager) StreamTree(streamID string) (*tree.StreamManifest, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.activeRoom == nil {
		return nil, false
	}
	return m.activeRoom.StreamEngine.GetStreamManifest(streamID)
}

func (m *CallManager) registerStreamLocked(room *ActiveRoomState, ann signaling.StreamAnnouncePayload) {
	q := tree.StreamQuality{ID: ann.QualityID, Width: ann.Width, Height: ann.Height, FPS: ann.FPS, BitrateKbps: ann.BitrateKbps, Codec: ann.Codec}
	if cur, ok := room.StreamEngine.GetStreamManifest(ann.StreamID); ok {
		if cur.Broadcaster != ann.Broadcaster {
			m.warnOnce("streamowner:"+ann.StreamID, "Refused stream registration: the ID belongs to another broadcaster",
				"stream", ann.StreamID, "owner", calllog.Peer(cur.Broadcaster), "claimant", calllog.Peer(ann.Broadcaster))
			return // stream IDs are owned by their first broadcaster
		}
	} else if _, err := room.StreamEngine.RegisterStream(ann.StreamID, ann.Broadcaster, ann.MediaType, ann.BitrateKbps, ann.Profile); err != nil {
		m.logger.Warn("Stream registration failed", "stream", ann.StreamID, "broadcaster", calllog.Peer(ann.Broadcaster), "err", err)
		return
	} else {
		m.treeLog.Info("Stream registered", "stream", ann.StreamID, "broadcaster", calllog.Peer(ann.Broadcaster),
			"media", ann.MediaType, "quality", ann.QualityID, "kbps", ann.BitrateKbps)
	}
	_ = room.StreamEngine.SetStreamQuality(ann.StreamID, q) // the stream was found or created just above
	room.announces[ann.StreamID] = ann
	m.floodLocked(room, signaling.ActionStreamAnnounce, ann)
	if sm, ok := room.StreamEngine.GetStreamManifest(ann.StreamID); ok {
		m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
	}
	m.notifyAnnounceLocked(ann)
}

func (m *CallManager) unregisterStreamLocked(room *ActiveRoomState, streamID string, by types.PeerID) {
	cur, ok := room.StreamEngine.GetStreamManifest(streamID)
	if !ok {
		m.logger.Debug("Ignored unregister of an unknown stream", "stream", streamID, "by", calllog.Peer(by))
		return
	}
	if cur.Broadcaster != by {
		m.warnOnce("unreg:"+streamID, "Refused stream unregister from a peer that does not broadcast it",
			"stream", streamID, "by", calllog.Peer(by), "broadcaster", calllog.Peer(cur.Broadcaster))
		return
	}
	m.treeLog.Info("Stream unregistered", "stream", streamID, "broadcaster", calllog.Peer(by))
	m.floodLocked(room, signaling.ActionStreamClose, streamID)
	m.closeStreamLocalLocked(room, streamID)
}

func (m *CallManager) subscribeLocked(room *ActiveRoomState, sub signaling.StreamSubscribePayload) {
	sm, err := room.StreamEngine.Subscribe(sub.StreamID, sub.Subscriber, sub.Profile)
	if err != nil {
		if errors.Is(err, tree.ErrStreamNotFound) {
			// the subscribe overtook the registration; the subscriber's reconcile retries it
			m.debugOnce("sub:"+sub.StreamID+":"+sub.Subscriber.String(), "Subscribe to a stream not registered yet",
				"stream", sub.StreamID, "subscriber", calllog.Peer(sub.Subscriber))
		} else {
			m.treeLog.Warn("Stream subscribe failed", "stream", sub.StreamID, "subscriber", calllog.Peer(sub.Subscriber), "err", err)
		}
		return
	}
	m.treeLog.Info("Stream subscriber attached", "stream", sub.StreamID, "subscriber", calllog.Peer(sub.Subscriber),
		"parent", calllog.Peer(sm.ParentMap[sub.Subscriber]), "subscribers", len(sm.Subscribers), "overflow", len(sm.Overflow))
	m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
	m.afterStreamTreeChangeLocked(room, sm)
	if sub.Subscriber == m.selfID {
		m.requestKeyframeLocked(room, sub.StreamID, time.Now())
	}
}

func (m *CallManager) unsubscribeLocked(room *ActiveRoomState, streamID string, sub types.PeerID) {
	sm, err := room.StreamEngine.Unsubscribe(streamID, sub)
	if err != nil {
		m.logger.Debug("Ignored unsubscribe", "stream", streamID, "subscriber", calllog.Peer(sub), "err", err)
		return
	}
	m.treeLog.Info("Stream subscriber detached", "stream", streamID, "subscriber", calllog.Peer(sub), "subscribers", len(sm.Subscribers))
	m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
}

func (m *CallManager) applyStreamManifestLocked(room *ActiveRoomState, sm *tree.StreamManifest) {
	prevParent, wasAttached := room.StreamEngine.GetUpstream(sm.StreamID, m.selfID)
	if !room.StreamEngine.ApplyManifest(sm) {
		return
	}
	parent, attached := sm.ParentMap[m.selfID]
	switch {
	case attached && (!wasAttached || parent != prevParent):
		m.treeLog.Info("Stream feeder changed", "stream", sm.StreamID, "epoch", sm.Epoch, "parent", calllog.Peer(parent),
			"previous", calllog.Peer(prevParent), "watching", room.watching[sm.StreamID])
	case !attached && wasAttached && room.watching[sm.StreamID]:
		// reconcile subscribes again, but until then the stream is frozen
		m.treeLog.Warn("Dropped from the tree of a watched stream", "stream", sm.StreamID, "epoch", sm.Epoch, "previous", calllog.Peer(prevParent))
	}
	if attached && room.watching[sm.StreamID] && (!wasAttached || parent != prevParent) {
		// new path: ask for a keyframe instead of waiting for the next periodic one
		m.requestKeyframeLocked(room, sm.StreamID, time.Now())
	}
	m.afterStreamTreeChangeLocked(room, sm)
}

func (m *CallManager) afterStreamTreeChangeLocked(room *ActiveRoomState, sm *tree.StreamManifest) {
	if p, ok := sm.ParentMap[m.selfID]; ok {
		m.ensureConnectedLocked(p)
	}
	for _, c := range sm.DistributionTree[m.selfID] {
		m.ensureConnectedLocked(c)
	}
}

func (m *CallManager) closeStreamLocalLocked(room *ActiveRoomState, streamID string) {
	_, known := room.StreamEngine.GetStreamManifest(streamID)
	if _, ann := room.announces[streamID]; known || ann {
		m.logger.Info("Stream closed", "stream", streamID, "was_watching", room.watching[streamID])
	}
	room.StreamEngine.UnregisterStream(streamID)
	room.Dispatcher.SetStreamWatching(streamID, false)
	m.releaseVideoSlotLocked(room, streamID)
	delete(room.watching, streamID)
	delete(room.announces, streamID)
	if cb := m.OnStreamClosed; cb != nil {
		m.later(func() { cb(streamID) })
	}
}
