package call

import (
	"errors"

	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// GetBridgeOffer retrieves the WebRTC SDP Offer from the local media loopback bridge.
func (m *CallManager) GetBridgeOffer() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.activeRoom == nil || m.activeRoom.Bridge == nil {
		err := errors.New("call room or media bridge not initialized")
		m.logger.Warn("UI asked for a media bridge offer without a bridge", "in_call", m.activeRoom != nil, "err", err)
		return "", err
	}
	return m.activeRoom.Bridge.CreateOffer() // the bridge logs its own failures
}

// SubmitBridgeAnswer sets the WebRTC SDP Answer from the Wails frontend to complete the loopback handshake.
func (m *CallManager) SubmitBridgeAnswer(sdpAnswer string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.activeRoom == nil || m.activeRoom.Bridge == nil {
		err := errors.New("call room or media bridge not initialized")
		m.logger.Warn("UI sent a media bridge answer without a bridge", "in_call", m.activeRoom != nil, "err", err)
		return err
	}
	return m.activeRoom.Bridge.SetRemoteAnswer(sdpAnswer)
}

// SetVADState notifies peers whether the local microphone is active.
func (m *CallManager) SetVADState(speaking bool, audioDb float32) {
	m.mu.Lock()
	room := m.activeRoom
	if room == nil {
		m.mu.Unlock()
		return
	}
	m.floodLocked(room, signaling.ActionVADState, signaling.VADPayload{Speaker: m.selfID, Speaking: speaking, AudioDb: audioDb})
	if cb := m.OnActiveSpeaker; cb != nil {
		self := m.selfID
		m.later(func() { cb(self, speaking, audioDb) })
	}
	m.unlockAndFlush()
}

// PublishAudio sends one encoded audio unit (an RTP packet from the UI) to the room.
func (m *CallManager) PublishAudio(payload []byte) error {
	d := m.dispatcher()
	if d == nil {
		return ErrNotInCall
	}
	return d.BroadcastLocalAudio(payload)
}

// PublishVideo sends one encoded video unit of a local stream down its distribution tree.
func (m *CallManager) PublishVideo(streamID string, payload []byte) error {
	m.mu.RLock()
	room := m.activeRoom
	var local bool
	if room != nil {
		_, local = room.localStreams[streamID]
	}
	m.mu.RUnlock()
	if room == nil {
		return ErrNotInCall
	}
	if !local {
		return ErrNoLocalStream
	}
	return room.Dispatcher.BroadcastLocalVideo(streamID, payload)
}

// captureRole maps a stream media type onto the bridge transceiver that captures it.
func captureRole(mediaType string) string {
	if mediaType == "camera" {
		return bridge.RoleCamera
	}
	return bridge.RoleScreen
}

func (m *CallManager) onBridgeRTP(role string, payload []byte) {
	if role == bridge.RoleMic {
		if err := m.PublishAudio(payload); err != nil {
			m.publishFailed(role, "", err)
		}
		return
	}
	video := role
	if role == bridge.RoleScreenAudio {
		video = bridge.RoleScreen // the sound belongs to the screen stream
	}
	m.mu.RLock()
	streamID := ""
	if m.activeRoom != nil {
		streamID = m.activeRoom.localByRole[video]
	}
	m.mu.RUnlock()
	if streamID == "" {
		// the UI sends a capture the room was never told about (StartStream missing or failed)
		m.debugOnce("nostream:"+role, "Dropped UI capture with no stream started for it", "role", role)
		return
	}
	var err error
	if role == bridge.RoleScreenAudio {
		err = m.PublishStreamAudio(streamID, payload)
	} else {
		err = m.PublishVideo(streamID, payload)
	}
	if err != nil {
		m.publishFailed(role, streamID, err)
	}
}

// publishFailed logs a local unit that could not be sent, e.g. a video frame too large to seal.
func (m *CallManager) publishFailed(role, streamID string, err error) {
	if errors.Is(err, ErrNotInCall) {
		return // the capture outlives the call by a few packets
	}
	if errors.Is(err, ErrNoLocalStream) {
		// same after a stop; only worth a look if it goes on
		m.debugOnce("publish:"+role+":stopped", "Dropped local media of a stopped stream", "role", role, "stream", streamID)
		return
	}
	m.warnOnce("publish:"+role+":"+err.Error(), "Failed to send local media", "role", role, "stream", streamID, "err", err)
}

// PublishStreamAudio sends one encoded audio unit of a local stream (a screen's sound) down the
// stream's tree, to its viewers only.
func (m *CallManager) PublishStreamAudio(streamID string, payload []byte) error {
	m.mu.RLock()
	room := m.activeRoom
	var local bool
	if room != nil {
		_, local = room.localStreams[streamID]
	}
	m.mu.RUnlock()
	if room == nil {
		return ErrNotInCall
	}
	if !local {
		return ErrNoLocalStream
	}
	return room.Dispatcher.BroadcastLocalStreamAudio(streamID, payload)
}

func (m *CallManager) onBridgeSlot(ev bridge.SlotEvent) {
	m.mu.RLock()
	cb := m.OnMediaSlot
	m.mu.RUnlock()
	if cb != nil {
		cb(ev)
	}
}

// MediaSlots lists which remote source plays on each UI track (for a reloaded UI).
func (m *CallManager) MediaSlots() []bridge.SlotEvent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.activeRoom == nil || m.activeRoom.Bridge == nil {
		return nil
	}
	return m.activeRoom.Bridge.Slots()
}

// MediaStats returns media counters of the active room.
func (m *CallManager) MediaStats() media.DispatcherStats {
	if d := m.dispatcher(); d != nil {
		return d.Stats()
	}
	return media.DispatcherStats{}
}

// KeyframesServed counts keyframe requests that reached this node as a broadcaster.
func (m *CallManager) KeyframesServed() uint64 { return m.keyframesServed.Load() }

func (m *CallManager) handleIncomingAudio(payload []byte, from types.PeerID) error {
	d := m.dispatcher()
	if d == nil {
		return nil
	}
	return d.ProcessIncomingAudio(payload, from)
}

func (m *CallManager) handleIncomingVideo(payload []byte, from types.PeerID) error {
	d := m.dispatcher()
	if d == nil {
		return nil
	}
	return d.ProcessIncomingVideo(payload, from)
}

// OnLocalAudio implements media.LocalMediaConsumer.
func (m *CallManager) OnLocalAudio(packet *media.MediaPacket) {
	m.mu.RLock()
	room, tap := m.activeRoom, m.OnMedia
	m.mu.RUnlock()
	if tap != nil {
		tap(packet)
	}
	if room != nil && room.Bridge != nil {
		_ = room.Bridge.PushRemoteAudio(packet) // the bridge logs failures, rate limited
	}
}

// OnLocalVideo implements media.LocalMediaConsumer: a stream's video, or its sound.
func (m *CallManager) OnLocalVideo(packet *media.MediaPacket) {
	m.mu.RLock()
	room, tap := m.activeRoom, m.OnMedia
	m.mu.RUnlock()
	if tap != nil {
		tap(packet)
	}
	if room == nil || room.Bridge == nil {
		return
	}
	// the bridge logs push failures, rate limited
	if packet.Kind == media.FrameStreamAudio {
		_ = room.Bridge.PushRemoteStreamAudio(packet)
		return
	}
	_ = room.Bridge.PushRemoteVideo(packet)
}
