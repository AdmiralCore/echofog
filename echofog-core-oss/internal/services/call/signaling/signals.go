package signaling

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type SignalAction string

const (
	// Flooded over the voice tree, originated by the coordinator.
	ActionTreeManifest         SignalAction = "tree_manifest"
	ActionCoordinatorHeartbeat SignalAction = "coordinator_heartbeat"
	ActionStreamAnnounce       SignalAction = "stream_announce"
	ActionStreamClose          SignalAction = "stream_close"
	ActionStreamTreeUpdate     SignalAction = "stream_tree_update"
	// Flooded, originated by the peer the payload is about.
	ActionCoordinatorElection SignalAction = "coordinator_election"
	ActionVADState            SignalAction = "vad_state"

	// Unicast member -> coordinator.
	ActionJoin             SignalAction = "join"
	ActionLeave            SignalAction = "leave"
	ActionMemberHeartbeat  SignalAction = "member_heartbeat"
	ActionParentLost       SignalAction = "parent_lost"
	ActionStreamRegister   SignalAction = "stream_register"
	ActionStreamUnregister SignalAction = "stream_unregister"
	ActionStreamSubscribe  SignalAction = "stream_subscribe"
	ActionStreamUnsub      SignalAction = "stream_unsubscribe"
	ActionPing             SignalAction = "ping"
	ActionSyncRequest      SignalAction = "sync_request"
	ActionStreamReparent   SignalAction = "stream_reparent"
	ActionReport           SignalAction = "report"

	// Unicast between neighbours.
	ActionPong            SignalAction = "pong"
	ActionRoomSync        SignalAction = "room_sync"
	ActionAdopt           SignalAction = "adopt"
	ActionKeyframeRequest SignalAction = "keyframe_request"
	ActionKicked          SignalAction = "kicked"
	ActionNack            SignalAction = "nack"
)

// Kept for source compatibility with older callers.
const ActionStreamUnsubscribe = ActionStreamUnsub

var (
	ErrBadSignature  = errors.New("call signal signature is invalid")
	ErrSenderMissing = errors.New("call signal is not signed by its sender")
)

// IsFlooded reports whether an action travels room-wide over the voice tree.
func IsFlooded(a SignalAction) bool {
	switch a {
	case ActionTreeManifest, ActionCoordinatorHeartbeat, ActionStreamAnnounce, ActionStreamClose,
		ActionStreamTreeUpdate, ActionCoordinatorElection, ActionVADState:
		return true
	}
	return false
}

// CallSignalEnvelope wraps any signaling payload. Flooded envelopes are relayed byte-for-byte,
// so the signature lets every hop check who really originated them.
type CallSignalEnvelope struct {
	Action       SignalAction        `json:"action"`
	CallID       string              `json:"call_id"`
	Sender       types.PeerID        `json:"sender"`
	SenderPubKey types.PeerPublicKey `json:"sender_pub,omitempty"`
	MsgID        uint64              `json:"msg_id"`
	Epoch        uint64              `json:"epoch"`
	Timestamp    int64               `json:"timestamp"`
	Payload      []byte              `json:"payload"`
	Signature    []byte              `json:"sig,omitempty"`
}

// Signer produces identity signatures; *swarm.Swarm satisfies it.
type Signer interface {
	CreateSignature(data []byte) ([]byte, error)
	GetMyPubKey() types.PeerPublicKey
}

// StreamAnnouncePayload notifies the room of an active video or screen stream.
type StreamAnnouncePayload struct {
	StreamID    string               `json:"stream_id"`
	Broadcaster types.PeerID         `json:"broadcaster"`
	MediaType   string               `json:"media_type"` // "screen" | "camera"
	BitrateKbps int                  `json:"bitrate_kbps"`
	Width       int                  `json:"width,omitempty"`
	Height      int                  `json:"height,omitempty"`
	FPS         int                  `json:"fps,omitempty"`
	Codec       string               `json:"codec,omitempty"`
	QualityID   string               `json:"quality_id,omitempty"`
	Profile     tree.PeerNodeProfile `json:"profile"`
}

// StreamSubscribePayload registers a subscriber for a specific stream.
type StreamSubscribePayload struct {
	StreamID   string               `json:"stream_id"`
	Subscriber types.PeerID         `json:"subscriber"`
	Profile    tree.PeerNodeProfile `json:"profile"`
}

// JoinPayload asks the coordinator for a place in the tree and proves knowledge of the room key.
type JoinPayload struct {
	Profile tree.PeerNodeProfile `json:"profile"`
	Proof   []byte               `json:"proof"`
}

// RoomSyncPayload brings a joiner or a lagging member up to date in one message.
type RoomSyncPayload struct {
	Voice     *tree.TreeManifest      `json:"voice"`
	Streams   []*tree.StreamManifest  `json:"streams"`
	Announces []StreamAnnouncePayload `json:"announces"`
}

// ReparentPayload asks the coordinator to feed a stream to the sender through another relay.
type ReparentPayload struct {
	StreamID string       `json:"stream_id"`
	Avoid    types.PeerID `json:"avoid"`
	Reason   string       `json:"reason"` // "silent" (watchdog) | "user" (switch relay button)
}

// ReportPayload flags a member as sending spam or garbage media.
// NackPayload asks a node's parent on a stream tree to resend lost units from its retransmit ring.
type NackPayload struct {
	StreamID string            `json:"stream_id"`
	Lost     []media.NackEntry `json:"lost"`
}

type ReportPayload struct {
	Target types.PeerID `json:"target"`
	Reason string       `json:"reason"`
}

// VADPayload communicates speaker state to illuminate avatar and route audio.
type VADPayload struct {
	Speaker  types.PeerID `json:"speaker"`
	Speaking bool         `json:"speaking"`
	AudioDb  float32      `json:"audio_db"`
}

// CoordinatorHeartbeatPayload carries room liveness and a digest of the room state. Signals are
// delivered unordered, so members compare the digest with their replicas and ask for a
// RoomSync when they missed or misordered an update.
type CoordinatorHeartbeatPayload struct {
	Coordinator types.PeerID      `json:"coordinator"`
	Epoch       uint64            `json:"epoch"`
	Streams     map[string]uint64 `json:"streams,omitempty"` // stream ID -> tree epoch
}

// JoinProof binds a member identity to the room key without revealing the key.
func JoinProof(roomKey []byte, callID string, member types.PeerID) []byte {
	mac := hmac.New(sha256.New, roomKey)
	mac.Write([]byte("echofog-call-join\x00"))
	mac.Write([]byte(callID))
	mac.Write(member[:])
	return mac.Sum(nil)
}

// VerifyJoinProof checks a JoinProof in constant time.
func VerifyJoinProof(roomKey []byte, callID string, member types.PeerID, proof []byte) bool {
	return hmac.Equal(JoinProof(roomKey, callID, member), proof)
}

// EncodeSignal builds an unsigned envelope (offline tests and legacy callers).
func EncodeSignal(action SignalAction, callID string, sender types.PeerID, epoch uint64, data any) ([]byte, error) {
	return EncodeSignedSignal(nil, action, callID, sender, epoch, data)
}

// EncodeSignedSignal builds an envelope and signs it when a signer is provided.
func EncodeSignedSignal(signer Signer, action SignalAction, callID string, sender types.PeerID, epoch uint64, data any) ([]byte, error) {
	var payloadBytes []byte
	if data != nil {
		var err error
		if payloadBytes, err = json.Marshal(data); err != nil {
			return nil, err
		}
	}

	env := CallSignalEnvelope{
		Action:    action,
		CallID:    callID,
		Sender:    sender,
		MsgID:     rand.Uint64(), // #nosec G404 -- an id for de-duplication, not a secret
		Epoch:     epoch,
		Timestamp: time.Now().UnixMilli(),
		Payload:   payloadBytes,
	}
	if signer != nil {
		env.SenderPubKey = signer.GetMyPubKey()
		sig, err := signer.CreateSignature(env.signingBytes())
		if err != nil {
			return nil, err
		}
		env.Signature = sig
	}
	return json.Marshal(env)
}

func (e *CallSignalEnvelope) signingBytes() []byte {
	c := *e
	c.Signature = nil
	b, _ := json.Marshal(c)
	return b
}

// Verify checks that the envelope was signed by the identity behind Sender.
func (e *CallSignalEnvelope) Verify() error {
	if len(e.Signature) == 0 || e.SenderPubKey == (types.PeerPublicKey{}) {
		return ErrSenderMissing
	}
	if types.PeerPubKeyToID(e.SenderPubKey) != e.Sender {
		return ErrBadSignature
	}
	if !ed25519.Verify(e.SenderPubKey[:], e.signingBytes(), e.Signature) {
		return ErrBadSignature
	}
	return nil
}

func DecodeSignal(data []byte) (*CallSignalEnvelope, error) {
	if len(data) == 0 {
		return nil, errors.New("empty signal data")
	}
	var env CallSignalEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	return &env, nil
}
