package call

import (
	"fmt"
	"time"

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
		return ErrNotInCall
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
		return ErrNotInCall
	}
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
		return ErrNotWatching
	}
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
			return // stream IDs are owned by their first broadcaster
		}
	} else if _, err := room.StreamEngine.RegisterStream(ann.StreamID, ann.Broadcaster, ann.MediaType, ann.BitrateKbps, ann.Profile); err != nil {
		return
	}
	_ = room.StreamEngine.SetStreamQuality(ann.StreamID, q)
	room.announces[ann.StreamID] = ann
	m.floodLocked(room, signaling.ActionStreamAnnounce, ann)
	if sm, ok := room.StreamEngine.GetStreamManifest(ann.StreamID); ok {
		m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
	}
	m.notifyAnnounceLocked(ann)
}

func (m *CallManager) unregisterStreamLocked(room *ActiveRoomState, streamID string, by types.PeerID) {
	cur, ok := room.StreamEngine.GetStreamManifest(streamID)
	if !ok || cur.Broadcaster != by {
		return
	}
	m.floodLocked(room, signaling.ActionStreamClose, streamID)
	m.closeStreamLocalLocked(room, streamID)
}

func (m *CallManager) subscribeLocked(room *ActiveRoomState, sub signaling.StreamSubscribePayload) {
	sm, err := room.StreamEngine.Subscribe(sub.StreamID, sub.Subscriber, sub.Profile)
	if err != nil {
		return
	}
	m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
	m.afterStreamTreeChangeLocked(room, sm)
	if sub.Subscriber == m.selfID {
		m.requestKeyframeLocked(room, sub.StreamID, time.Now())
	}
}

func (m *CallManager) unsubscribeLocked(room *ActiveRoomState, streamID string, sub types.PeerID) {
	sm, err := room.StreamEngine.Unsubscribe(streamID, sub)
	if err != nil {
		return
	}
	m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
}

func (m *CallManager) applyStreamManifestLocked(room *ActiveRoomState, sm *tree.StreamManifest) {
	prevParent, wasAttached := room.StreamEngine.GetUpstream(sm.StreamID, m.selfID)
	if !room.StreamEngine.ApplyManifest(sm) {
		return
	}
	parent, attached := sm.ParentMap[m.selfID]
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
	room.StreamEngine.UnregisterStream(streamID)
	room.Dispatcher.SetStreamWatching(streamID, false)
	m.releaseVideoSlotLocked(room, streamID)
	delete(room.watching, streamID)
	delete(room.announces, streamID)
	if cb := m.OnStreamClosed; cb != nil {
		m.later(func() { cb(streamID) })
	}
}
