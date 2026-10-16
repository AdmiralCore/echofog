package call

import (
	"context"
	"encoding/json"
	"errors"
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
	// keyframeServeInterval: how often the broadcaster's encoder makes an IDR on request; each one
	// is huge at 4K, and the frames right after it get starved into blur.
	keyframeServeInterval = time.Second
	seenFloodTTL          = 10 * time.Second
	syncRequestInterval   = time.Second
	connectRetryInterval  = 2 * time.Second
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
	sendRate       sendRateState

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

// Name identifies this service (it implements the node's service interface).
func (m *CallManager) Name() string { return "CallManager" }

// Start is a no-op: the manager does its work only once a call is joined.
func (m *CallManager) Start(ctx context.Context) error { return nil }

// Stop leaves the current room, if any, when the node shuts down.
func (m *CallManager) Stop() error { return m.LeaveRoom() }

// SetPeerConnector lets the call layer dial tree neighbours it is not connected to yet.
func (m *CallManager) SetPeerConnector(c PeerConnector) {
	m.mu.Lock()
	m.connector = c
	m.mu.Unlock()
}

func (m *CallManager) dispatcher() *media.MediaDispatcher {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.activeRoom == nil {
		return nil
	}
	return m.activeRoom.Dispatcher
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
