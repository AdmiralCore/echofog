package call

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/crypto"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/events"
)

var (
	ErrCallAlreadyActive = errors.New("call session is already active")
	ErrNotInCall         = errors.New("not currently in a call")
	ErrInvalidRoomKey    = errors.New("room key must be 32 bytes")
	ErrSpoofedSender     = errors.New("call signal sender does not match the transport peer")
	ErrNoLocalStream     = errors.New("stream is not broadcast by this node")
	ErrNotWatching       = errors.New("this node does not receive that stream")
	ErrSelfReport        = errors.New("you cannot report yourself")
)

// Timing of the room protocol. The coordinator heartbeat doubles as the parent-liveness pulse
// because it is flooded down the voice tree.
const (
	tickInterval            = 100 * time.Millisecond
	coordHeartbeatInterval  = 200 * time.Millisecond
	memberHeartbeatInterval = 500 * time.Millisecond
	memberTimeout           = 2500 * time.Millisecond
	parentTimeout           = 800 * time.Millisecond
	coordinatorTimeout      = 2000 * time.Millisecond
	electionWait            = 1500 * time.Millisecond
	joinRetryInterval       = time.Second
	reconcileInterval       = time.Second
	pingInterval            = 300 * time.Millisecond
	keyframeMinInterval     = 300 * time.Millisecond
	seenFloodTTL            = 10 * time.Second
	syncRequestInterval     = time.Second
	connectRetryInterval    = 2 * time.Second
	// streamFeederTimeout: feeders pulse every 250 ms, so 700 ms of silence means the link is dead.
	streamFeederTimeout = 700 * time.Millisecond
	reparentRetry       = time.Second
	reportWindow        = 10 * time.Minute
)

// PeerConnector dials a room member that is not directly connected yet.
type PeerConnector func(ctx context.Context, peer types.PeerID) error

type floodKey struct {
	sender types.PeerID
	msgID  uint64
}

// ActiveRoomState tracks the state of the connected room.
type ActiveRoomState struct {
	CallID         string
	Coordinator    types.PeerID
	IsCoordinator  bool
	Epoch          uint64
	RoomKey        []byte
	VoiceManifest  *tree.TreeManifest
	StreamEngine   *tree.StreamTreeEngine
	Dispatcher     *media.MediaDispatcher
	Bridge         *bridge.LocalMediaBridge
	LastCoordPulse time.Time // any proof the coordinator is alive
	LastTreePulse  time.Time // coordinator heartbeat received from our upstream
	ConnectedPeers map[types.PeerID]tree.PeerNodeProfile
	MyProfile      tree.PeerNodeProfile
	Cancel         context.CancelFunc

	joined, everJoined, usingStandby bool

	memberSeen        map[types.PeerID]time.Time
	reportedLost      map[types.PeerID]time.Time                  // coordinator: relays whose children lost them
	reports           map[types.PeerID]map[types.PeerID]time.Time // coordinator: target -> reporter -> when
	banned            map[types.PeerID]bool                       // coordinator: kicked members
	reparentAsked     map[string]time.Time                        // member: stream -> last reparent request
	suspects          map[types.PeerID]bool
	candidate         types.PeerID
	candidateDeadline time.Time

	localStreams map[string]signaling.StreamAnnouncePayload
	localByRole  map[string]string // bridge capture role ("camera"/"screen") -> local stream ID
	watching     map[string]bool
	announces    map[string]signaling.StreamAnnouncePayload

	seen           map[floodKey]time.Time
	keyframeSent   map[string]time.Time
	keyframeServed map[string]time.Time

	lastJoin, lastMemberHB, lastCoordHB, lastPing, lastReconcile, lastSeenGC, lastSyncReq time.Time
}

// RoomSnapshot is a read-only view of the room, for UI and tests.
type RoomSnapshot struct {
	CallID        string
	Coordinator   types.PeerID
	IsCoordinator bool
	Epoch         uint64
	Joined        bool
	Members       []types.PeerID
	Streams       []string
}

// CallManager orchestrates Voice Backbone Trees and On-Demand Multi-Stream Video Trees.
type CallManager struct {
	mu     sync.RWMutex
	logger *slog.Logger

	selfID     types.PeerID
	swarm      *swarm.Swarm
	sender     media.FrameSender
	signer     signaling.Signer
	sesManager *swarm.SessionManager
	crypto     *crypto.CryptoEngine

	activeRoom *ActiveRoomState
	emitter    events.Emitter

	connector  PeerConnector
	connecting map[types.PeerID]time.Time

	pending []func()

	keyframesServed atomic.Uint64

	// UI / Application Callbacks
	OnActiveSpeaker      func(speaker types.PeerID, speaking bool, db float32)
	OnStreamAnnounced    func(streamID string, broadcaster types.PeerID, mediaType string, bitrate int)
	OnStreamInfo         func(info signaling.StreamAnnouncePayload)
	OnStreamClosed       func(streamID string)
	OnCoordinatorChanged func(newCoordinator types.PeerID, epoch uint64)
	OnTopologyUpdated    func(manifest *tree.TreeManifest)
	// OnKeyframeRequest fires on the broadcaster when a viewer needs a fresh keyframe.
	OnKeyframeRequest func(streamID string)
	// OnMedia receives every decrypted unit delivered to this node (recording, tests, SDK).
	OnMedia func(packet *media.MediaPacket)
	// OnMediaSlot reports which remote speaker/stream plays on which UI track.
	OnMediaSlot func(ev bridge.SlotEvent)
	// OnKicked fires when the room voted this node out.
	OnKicked func(reason string)
	// OnMemberKicked fires on the coordinator when it removes a reported member.
	OnMemberKicked func(peer types.PeerID)
}

// NewCallManager initializes the CallManager with swarm integration.
func NewCallManager(s *swarm.Swarm, ses *swarm.SessionManager, crypt *crypto.CryptoEngine, selfID types.PeerID) *CallManager {
	var (
		sender media.FrameSender
		signer signaling.Signer
	)
	if s != nil {
		sender, signer = s, s
	}
	cm := newCallManager(sender, signer, selfID)
	cm.swarm, cm.sesManager, cm.crypto = s, ses, crypt

	if s != nil {
		s.RegisterFastFrameHandler(net_types.TypeCallSignaling, cm.handleIncomingSignaling)
		s.RegisterFastFrameHandler(net_types.TypeCallMediaAudio, cm.handleIncomingAudio)
		s.RegisterFastFrameHandler(net_types.TypeCallMediaVideo, cm.handleIncomingVideo)
	}
	return cm
}

// newCallManager builds a manager over an arbitrary transport. A nil signer disables signal
// signatures, which is only acceptable for in-memory tests.
func newCallManager(sender media.FrameSender, signer signaling.Signer, selfID types.PeerID) *CallManager {
	return &CallManager{
		logger:     slog.Default().With("module", "call_manager"),
		selfID:     selfID,
		sender:     sender,
		signer:     signer,
		connecting: make(map[types.PeerID]time.Time),
	}
}

func (m *CallManager) Name() string { return "CallManager" }

func (m *CallManager) Start(ctx context.Context) error { return nil }

func (m *CallManager) Stop() error { return m.LeaveRoom() }

// SetPeerConnector lets the call layer dial tree neighbours it is not connected to yet.
func (m *CallManager) SetPeerConnector(c PeerConnector) {
	m.mu.Lock()
	m.connector = c
	m.mu.Unlock()
}

// JoinRoom enters a group call with a specified coordinator and room E2EE key.
func (m *CallManager) JoinRoom(callID string, coordinator types.PeerID, roomKey []byte, profile tree.PeerNodeProfile) error {
	if len(roomKey) != 32 {
		return ErrInvalidRoomKey
	}

	m.mu.Lock()
	if m.activeRoom != nil {
		m.mu.Unlock()
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
	}

	room.Dispatcher = media.NewMediaDispatcher(m.selfID, m.sender, m, engine)
	if err := room.Dispatcher.SetRoomKey(roomKey); err != nil {
		cancel()
		room.Dispatcher.Close()
		m.mu.Unlock()
		return err
	}
	room.Dispatcher.OnVideoLoss = func(streamID string, _ types.PeerID) { m.requestKeyframe(streamID) }

	if b, err := bridge.NewLocalMediaBridge(bridge.Options{
		OnLocalRTP:       m.onBridgeRTP,
		OnSlot:           m.onBridgeSlot,
		OnKeyframeNeeded: m.requestKeyframe,
	}); err == nil {
		room.Bridge = b
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

	m.logger.Info("Joined call room", "call_id", callID, "coordinator", coordinator.String(), "is_coord", room.IsCoordinator)
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
			sender := m.sender
			for _, t := range targets {
				go func(t types.PeerID) { _ = sender.SendRawFrame(t, net_types.TypeCallSignaling, leave) }(t)
			}
		}
	}

	room.Cancel()
	room.Dispatcher.Close()
	if room.Bridge != nil {
		_ = room.Bridge.Close()
	}
	m.activeRoom = nil

	m.logger.Info("Left call room", "call_id", room.CallID)
	m.emit("call:session_ended", map[string]any{"call_id": room.CallID})
	m.mu.Unlock()
	return nil
}

// GetBridgeOffer retrieves the WebRTC SDP Offer from the local media loopback bridge.
func (m *CallManager) GetBridgeOffer() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.activeRoom == nil || m.activeRoom.Bridge == nil {
		return "", errors.New("call room or media bridge not initialized")
	}
	return m.activeRoom.Bridge.CreateOffer()
}

// SubmitBridgeAnswer sets the WebRTC SDP Answer from the Wails frontend to complete the loopback handshake.
func (m *CallManager) SubmitBridgeAnswer(sdpAnswer string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.activeRoom == nil || m.activeRoom.Bridge == nil {
		return errors.New("call room or media bridge not initialized")
	}
	return m.activeRoom.Bridge.SetRemoteAnswer(sdpAnswer)
}

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
	m.toCoordinatorLocked(room, signaling.ActionReport, signaling.ReportPayload{Target: peer, Reason: reason})
	m.unlockAndFlush()
	return nil
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
		_ = m.PublishAudio(payload)
		return
	}
	m.mu.RLock()
	streamID := ""
	if m.activeRoom != nil {
		streamID = m.activeRoom.localByRole[role]
	}
	m.mu.RUnlock()
	if streamID != "" {
		_ = m.PublishVideo(streamID, payload)
	}
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

// releaseVideoSlotLocked frees the playback track after the lock is dropped: the bridge reports
// the release through OnMediaSlot, which may call back into the manager.
func (m *CallManager) releaseVideoSlotLocked(room *ActiveRoomState, streamID string) {
	if br := room.Bridge; br != nil {
		m.later(func() { br.ReleaseVideo(streamID) })
	}
}

func (m *CallManager) dispatcher() *media.MediaDispatcher {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.activeRoom == nil {
		return nil
	}
	return m.activeRoom.Dispatcher
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

// StreamTree returns this node's replica of a stream distribution tree.
func (m *CallManager) StreamTree(streamID string) (*tree.StreamManifest, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.activeRoom == nil {
		return nil, false
	}
	return m.activeRoom.StreamEngine.GetStreamManifest(streamID)
}

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
		_ = room.Bridge.PushRemoteAudio(packet)
	}
}

// OnLocalVideo implements media.LocalMediaConsumer.
func (m *CallManager) OnLocalVideo(packet *media.MediaPacket) {
	m.mu.RLock()
	room, tap := m.activeRoom, m.OnMedia
	m.mu.RUnlock()
	if tap != nil {
		tap(packet)
	}
	if room != nil && room.Bridge != nil {
		_ = room.Bridge.PushRemoteVideo(packet)
	}
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

	case signaling.ActionKeyframeRequest:
		var streamID string
		if decodePayload(env.Payload, &streamID) == nil {
			m.handleKeyframeRequestLocked(room, streamID, now)
		}
	}
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

func (m *CallManager) removeMemberLocked(room *ActiveRoomState, peer types.PeerID) {
	delete(room.ConnectedPeers, peer)
	delete(room.memberSeen, peer)
	m.logger.Info("Removing silent call member", "peer", peer.String())
	m.emit("call:member_removed", map[string]any{"call_id": room.CallID, "peer": peer.String()})

	m.rebuildVoiceLocked(room)
	changed, closed := room.StreamEngine.RemovePeer(peer)
	for _, sm := range changed {
		m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
	}
	for _, id := range closed {
		m.floodLocked(room, signaling.ActionStreamClose, id)
		m.closeStreamLocalLocked(room, id)
	}
	if ob := room.Dispatcher.Outbox(); ob != nil {
		ob.Forget(peer)
	}
}

func (m *CallManager) rebuildVoiceLocked(room *ActiveRoomState) {
	profiles := make([]tree.PeerNodeProfile, 0, len(room.ConnectedPeers))
	for _, p := range room.ConnectedPeers {
		profiles = append(profiles, p)
	}
	room.Epoch++
	manifest := tree.BuildVoiceTopology(room.CallID, room.Epoch, m.selfID, profiles)
	m.applyVoiceManifestLocked(room, manifest, time.Now())
	m.floodLocked(room, signaling.ActionTreeManifest, manifest)
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

// becomeCoordinatorLocked takes over the room after an election this node won.
func (m *CallManager) becomeCoordinatorLocked(room *ActiveRoomState, now time.Time) {
	old := room.Coordinator
	room.Coordinator, room.IsCoordinator = m.selfID, true
	room.candidate = types.PeerID{}
	room.Epoch++

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

	m.logger.Warn("Took over as call coordinator", "call_id", room.CallID, "previous", old.String(), "epoch", room.Epoch)
	m.floodLocked(room, signaling.ActionCoordinatorElection, m.selfID)
	m.rebuildVoiceLocked(room)

	for _, sm := range room.StreamEngine.Snapshot() {
		if _, alive := room.ConnectedPeers[sm.Broadcaster]; !alive {
			m.floodLocked(room, signaling.ActionStreamClose, sm.StreamID)
			m.closeStreamLocalLocked(room, sm.StreamID)
			continue
		}
		for _, sub := range sm.Subscribers {
			if _, alive := room.ConnectedPeers[sub]; !alive {
				_, _ = room.StreamEngine.Unsubscribe(sm.StreamID, sub)
			}
		}
		if rebuilt, err := room.StreamEngine.Rebuild(sm.StreamID); err == nil {
			m.floodLocked(room, signaling.ActionStreamTreeUpdate, rebuilt)
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
	room.VoiceManifest = manifest
	room.Epoch = manifest.Epoch
	room.IsCoordinator = manifest.Coordinator == m.selfID
	room.LastCoordPulse, room.LastTreePulse = now, now
	room.usingStandby = false

	if !room.IsCoordinator {
		room.ConnectedPeers = make(map[types.PeerID]tree.PeerNodeProfile, len(manifest.Profiles))
		for id, p := range manifest.Profiles {
			room.ConnectedPeers[id] = p
		}
		_, inTree := manifest.ParentMap[m.selfID]
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

// reparentLocked (coordinator) moves sub away from a relay. A "silent" report also starts the
// fast liveness check of that relay, exactly like a voice ParentLost.
func (m *CallManager) reparentLocked(room *ActiveRoomState, sub types.PeerID, rp signaling.ReparentPayload, now time.Time) {
	if rp.Reason == "silent" {
		if _, member := room.memberSeen[rp.Avoid]; member {
			if _, already := room.reportedLost[rp.Avoid]; !already {
				room.reportedLost[rp.Avoid] = now
			}
		}
	}
	sm, err := room.StreamEngine.Reparent(rp.StreamID, sub, rp.Avoid)
	if err != nil {
		return
	}
	m.floodLocked(room, signaling.ActionStreamTreeUpdate, sm)
	m.afterStreamTreeChangeLocked(room, sm)
	m.emit("call:relay_changed", map[string]any{"call_id": room.CallID, "stream": rp.StreamID, "subscriber": sub.String(), "avoid": rp.Avoid.String(), "reason": rp.Reason})
}

// reportLocked (coordinator) records a spam report. The reporter is immediately fed around the
// target; the target is kicked once a third of the other members (at least two) agree.
func (m *CallManager) reportLocked(room *ActiveRoomState, reporter types.PeerID, rp signaling.ReportPayload, now time.Time) {
	target := rp.Target
	if target == reporter || target == m.selfID {
		return // the coordinator cannot vote itself out; members can still mute it locally
	}
	if _, member := room.ConnectedPeers[target]; !member {
		return
	}
	if _, member := room.ConnectedPeers[reporter]; !member {
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
	m.emit("call:member_reported", map[string]any{"call_id": room.CallID, "target": target.String(), "votes": votes, "needed": need})
	if votes < need {
		return
	}

	m.logger.Warn("Kicking reported call member", "peer", target.String(), "votes", votes)
	room.banned[target] = true
	delete(room.reports, target)
	if kicked, err := m.encodeLocked(room, signaling.ActionKicked, "reported for spam"); err == nil && m.sender != nil {
		sender := m.sender
		go func() { _ = sender.SendRawFrame(target, net_types.TypeCallSignaling, kicked) }()
	}
	m.removeMemberLocked(room, target)
	if cb := m.OnMemberKicked; cb != nil {
		m.later(func() { cb(target) })
	}
}

// ---------------------------------------------------------------------------------------------
// Keyframes

func (m *CallManager) requestKeyframe(streamID string) {
	m.mu.Lock()
	if room := m.activeRoom; room != nil {
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
		return
	}
	room.keyframeSent[streamID] = now
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
	if now.Sub(room.keyframeServed[streamID]) < keyframeMinInterval {
		return
	}
	room.keyframeServed[streamID] = now
	m.keyframesServed.Add(1)
	br, cb := room.Bridge, m.OnKeyframeRequest
	m.later(func() {
		if br != nil {
			_ = br.RequestKeyframe(role)
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
	if now.Sub(room.lastCoordHB) >= coordHeartbeatInterval {
		room.lastCoordHB = now
		hb := signaling.CoordinatorHeartbeatPayload{Coordinator: m.selfID, Epoch: room.Epoch, Streams: make(map[string]uint64)}
		for _, sm := range room.StreamEngine.Snapshot() {
			hb.Streams[sm.StreamID] = sm.Epoch
		}
		m.floodLocked(room, signaling.ActionCoordinatorHeartbeat, hb)
	}
	for p, seen := range room.memberSeen {
		if p != m.selfID && now.Sub(seen) > memberTimeout {
			m.removeMemberLocked(room, p)
		}
	}
	// A child reported its parent silent: drop the parent as soon as its heartbeats confirm it,
	// instead of waiting for the full member timeout. A heartbeat after the report means only
	// the link broke, so the parent stays.
	for p, reportedAt := range room.reportedLost {
		seen, member := room.memberSeen[p]
		switch {
		case !member || seen.After(reportedAt.Add(memberHeartbeatInterval)):
			delete(room.reportedLost, p)
		case now.Sub(seen) > parentTimeout:
			delete(room.reportedLost, p)
			m.removeMemberLocked(room, p)
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
		if room.Dispatcher.FeederSilence(streamID, feeder, now) < streamFeederTimeout {
			continue
		}
		if now.Sub(room.reparentAsked[streamID]) < reparentRetry {
			continue
		}
		room.reparentAsked[streamID] = now
		m.logger.Info("Stream feeder silent, asking for another relay", "stream", streamID, "feeder", feeder.String())
		m.toCoordinatorLocked(room, signaling.ActionStreamReparent, signaling.ReparentPayload{StreamID: streamID, Avoid: feeder, Reason: "silent"})
	}
}

func (m *CallManager) memberTickLocked(room *ActiveRoomState, now time.Time) {
	if !room.joined {
		if now.Sub(room.lastJoin) >= joinRetryInterval {
			m.ensureConnectedLocked(room.Coordinator)
			m.sendJoinLocked(room, now)
		}
	} else if now.Sub(room.lastMemberHB) >= memberHeartbeatInterval {
		room.lastMemberHB = now
		m.sendLocked(room, room.Coordinator, signaling.ActionMemberHeartbeat, m.joinPayloadLocked(room))
	}

	// Parent watchdog: no pulse from upstream means the parent (or the whole room) is gone.
	if room.joined && room.VoiceManifest != nil && !room.usingStandby && now.Sub(room.LastTreePulse) > parentTimeout {
		ass := room.VoiceManifest.ParentMap[m.selfID]
		if ass.StandbyParent != (types.PeerID{}) && ass.StandbyParent != ass.PrimaryParent {
			room.usingStandby = true
			room.LastTreePulse = now
			room.Dispatcher.UseStandbyParent(ass.StandbyParent)
			m.ensureConnectedLocked(ass.StandbyParent)
			m.sendLocked(room, ass.StandbyParent, signaling.ActionAdopt, nil)
			m.logger.Info("Voice parent silent, switched to standby", "parent", ass.PrimaryParent.String(), "standby", ass.StandbyParent.String())
		}
		m.sendLocked(room, room.Coordinator, signaling.ActionParentLost, ass.PrimaryParent)
	}

	// Coordinator watchdog: probe directly before declaring it dead.
	silent := now.Sub(room.LastCoordPulse)
	if silent > coordinatorTimeout/2 && now.Sub(room.lastPing) >= pingInterval {
		room.lastPing = now
		m.sendLocked(room, room.Coordinator, signaling.ActionPing, nil)
	}
	if room.everJoined && silent > coordinatorTimeout {
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

// electLocked runs the deterministic election. Only the winner announces itself; others wait,
// and if the winner stays silent it is suspected too and the next candidate is tried.
func (m *CallManager) electLocked(room *ActiveRoomState, now time.Time) {
	if room.candidate != (types.PeerID{}) {
		if now.Before(room.candidateDeadline) {
			return
		}
		room.suspects[room.candidate] = true
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
		return
	}
	if leader == m.selfID {
		m.becomeCoordinatorLocked(room, now)
		return
	}
	room.candidate, room.candidateDeadline = leader, now.Add(electionWait)
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
			m.logger.Debug("Could not reach call peer", "peer", peer.String(), "err", err)
		}
	}()
}

// later queues a callback to run after the manager lock is released, so UI handlers may
// safely call back into the manager.
func (m *CallManager) later(fn func()) { m.pending = append(m.pending, fn) }

func (m *CallManager) unlockAndFlush() {
	fns := m.pending
	m.pending = nil
	m.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func decodePayload[T any](data []byte, target *T) error {
	var val T
	if err := json.Unmarshal(data, &val); err != nil {
		return err
	}
	*target = val
	return nil
}

// SetEmitter attaches the node event bus for call telemetry
func (m *CallManager) SetEmitter(e events.Emitter) {
	m.mu.Lock()
	m.emitter = e
	m.mu.Unlock()
}

// emit is called with m.mu held, so it reads emitter without locking.
func (m *CallManager) emit(topic string, payload any) {
	if m.emitter != nil {
		m.emitter.Emit(topic, payload)
	}
}
