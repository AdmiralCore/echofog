// Package call implements the signalling of call/2 (docs/protocol/v2/services.md 9.1).
//
// A SIGNAL frame carries a Signal wrapper with the originator's device akey, the exact CallSignal
// bytes and their signature. Signals forwarded beyond their first hop are signed by the
// originator: sig = Ed25519(origin key, "echofog/call2/signal" || body). The signature is checked
// over the received bytes, never over a re-encoding, so a relay forwards the wrapper unchanged and
// a forged copy is dropped. Hop-local signals (ping, pong, NACK, reports, unicast requests to the
// coordinator) carry no signature: the session authenticates them. A join carries the joiner's
// fresh Ed25519 media signing key and a fresh 16-byte join_salt (services.md 9.1).
package call

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

const (
	// Version is the call/2 service version (versioning.md 3.4).
	Version uint64 = 2
	// TypeSignal is the only call/2 frame type (registries.md 4).
	TypeSignal uint64 = 0x40

	// SignalLabel is the signature context of forwarded call signals (registries.md 7.5,
	// services.md 9.1). The signature input is SignalLabel || exact CallSignal bytes.
	SignalLabel = "echofog/call2/signal"

	// JoinSaltLen is the length of a fresh join_salt (services.md 9.1).
	JoinSaltLen = 16
)

// Action is the wire enum of the v1 call actions (call/signaling/signals.go).
type Action = pb.Action

// The v1 actions as wire constants.
const (
	ActionTreeManifest         = pb.Action_ACTION_TREE_MANIFEST
	ActionCoordinatorHeartbeat = pb.Action_ACTION_COORDINATOR_HEARTBEAT
	ActionStreamAnnounce       = pb.Action_ACTION_STREAM_ANNOUNCE
	ActionStreamClose          = pb.Action_ACTION_STREAM_CLOSE
	ActionStreamTreeUpdate     = pb.Action_ACTION_STREAM_TREE_UPDATE
	ActionCoordinatorElection  = pb.Action_ACTION_COORDINATOR_ELECTION
	ActionVADState             = pb.Action_ACTION_VAD_STATE
	ActionJoin                 = pb.Action_ACTION_JOIN
	ActionLeave                = pb.Action_ACTION_LEAVE
	ActionMemberHeartbeat      = pb.Action_ACTION_MEMBER_HEARTBEAT
	ActionParentLost           = pb.Action_ACTION_PARENT_LOST
	ActionStreamRegister       = pb.Action_ACTION_STREAM_REGISTER
	ActionStreamUnregister     = pb.Action_ACTION_STREAM_UNREGISTER
	ActionStreamSubscribe      = pb.Action_ACTION_STREAM_SUBSCRIBE
	ActionStreamUnsubscribe    = pb.Action_ACTION_STREAM_UNSUBSCRIBE
	ActionPing                 = pb.Action_ACTION_PING
	ActionSyncRequest          = pb.Action_ACTION_SYNC_REQUEST
	ActionStreamReparent       = pb.Action_ACTION_STREAM_REPARENT
	ActionReport               = pb.Action_ACTION_REPORT
	ActionPong                 = pb.Action_ACTION_PONG
	ActionRoomSync             = pb.Action_ACTION_ROOM_SYNC
	ActionAdopt                = pb.Action_ACTION_ADOPT
	ActionKeyframeRequest      = pb.Action_ACTION_KEYFRAME_REQUEST
	ActionKicked               = pb.Action_ACTION_KICKED
	ActionNack                 = pb.Action_ACTION_NACK
	ActionStreamFeedback       = pb.Action_ACTION_STREAM_FEEDBACK
	ActionStreamWeak           = pb.Action_ACTION_STREAM_WEAK
	ActionMediaKey             = pb.Action_ACTION_MEDIA_KEY
	ActionMediaKeyAck          = pb.Action_ACTION_MEDIA_KEY_ACK
)

// Errors of the call signalling.
var (
	ErrMalformed           = errors.New("call: malformed signal")
	ErrBadSignature        = errors.New("call: bad signal signature")
	ErrMissingSignature    = errors.New("call: forwarded signal is not signed")
	ErrUnexpectedSignature = errors.New("call: hop-local signal is signed")
	ErrSpoofedSender       = errors.New("call: signal sender does not match its origin")
	ErrUnknownAction       = errors.New("call: unknown call action")
	ErrBadJoin             = errors.New("call: bad join signal")
	ErrNoSigner            = errors.New("call: no signal signer")
	ErrNoTransport         = errors.New("call: no call transport")
	ErrNoDatagrams         = errors.New("call: no datagram port")
	ErrDuplicate           = errors.New("call: duplicate signal")
	ErrTimeout             = errors.New("call: signal request timed out")
	ErrClosed              = errors.New("call: service closed")
)

var actionNames = map[Action]string{
	ActionTreeManifest:         "tree_manifest",
	ActionCoordinatorHeartbeat: "coordinator_heartbeat",
	ActionStreamAnnounce:       "stream_announce",
	ActionStreamClose:          "stream_close",
	ActionStreamTreeUpdate:     "stream_tree_update",
	ActionCoordinatorElection:  "coordinator_election",
	ActionVADState:             "vad_state",
	ActionJoin:                 "join",
	ActionLeave:                "leave",
	ActionMemberHeartbeat:      "member_heartbeat",
	ActionParentLost:           "parent_lost",
	ActionStreamRegister:       "stream_register",
	ActionStreamUnregister:     "stream_unregister",
	ActionStreamSubscribe:      "stream_subscribe",
	ActionStreamUnsubscribe:    "stream_unsubscribe",
	ActionPing:                 "ping",
	ActionSyncRequest:          "sync_request",
	ActionStreamReparent:       "stream_reparent",
	ActionReport:               "report",
	ActionPong:                 "pong",
	ActionRoomSync:             "room_sync",
	ActionAdopt:                "adopt",
	ActionKeyframeRequest:      "keyframe_request",
	ActionKicked:               "kicked",
	ActionNack:                 "nack",
	ActionStreamFeedback:       "stream_feedback",
	ActionStreamWeak:           "stream_weak",
	ActionMediaKey:             "media_key",
	ActionMediaKeyAck:          "media_key_ack",
}

var actionByName = func() map[string]Action {
	m := make(map[string]Action, len(actionNames))
	for a, n := range actionNames {
		m[n] = a
	}
	return m
}()

// V1Name returns the v1 action name, or "" for an unknown action.
func V1Name(a Action) string { return actionNames[a] }

// ActionByName maps a v1 action name to its wire value.
func ActionByName(name string) (Action, bool) {
	a, ok := actionByName[name]
	return a, ok
}

// IsForwarded reports whether a signal may travel beyond its first hop and therefore must be
// signed by its originator (services.md 9.1). The flooded v1 actions and the join are forwarded;
// hop-local liveness, loss recovery and unicast requests to the coordinator are not.
func IsForwarded(a Action) bool {
	switch a {
	case ActionTreeManifest, ActionCoordinatorHeartbeat, ActionStreamAnnounce,
		ActionStreamClose, ActionStreamTreeUpdate, ActionCoordinatorElection,
		ActionVADState, ActionJoin:
		return true
	}
	return false
}

// IsLossy reports whether a signal is duplicated on a CALL_SIGNAL datagram for low latency
// (services.md 9.1: NACK and reports).
func IsLossy(a Action) bool {
	switch a {
	case ActionNack, ActionReport, ActionStreamFeedback, ActionStreamWeak:
		return true
	}
	return false
}

// Signal is one parsed SIGNAL frame body.
type Signal struct {
	// Origin is the originator's device akey on a forwarded signal; empty on a hop-local one.
	Origin []byte
	// Body is the exact CallSignal bytes the signature covers.
	Body []byte
	// Sig is Ed25519(origin key, SignalLabel || Body).
	Sig []byte
	// Msg is the decoded body.
	Msg *pb.CallSignal
}

// Encode builds a SIGNAL frame body. With a non-nil origin the body is signed by sign over
// SignalLabel || body; with a nil origin the result is a hop-local signal with no signature.
func Encode(m *pb.CallSignal, origin keys.AKey, sign func([]byte) ([]byte, error)) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil signal", ErrMalformed)
	}
	body, err := objects.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: body: %v", ErrMalformed, err)
	}
	out := &pb.Signal{Body: body}
	if len(origin) > 0 {
		if _, err := keys.ParseKind(origin, keys.Ed25519); err != nil {
			return nil, fmt.Errorf("%w: origin: %v", ErrBadSignature, err)
		}
		if sign == nil {
			return nil, ErrNoSigner
		}
		sig, err := sign(append([]byte(SignalLabel), body...))
		if err != nil {
			return nil, fmt.Errorf("%w: sign: %v", ErrNoSigner, err)
		}
		out.Origin, out.Sig = origin, sig
	}
	raw, err := marshalSignal(out)
	if err != nil {
		return nil, fmt.Errorf("%w: wrapper: %v", ErrMalformed, err)
	}
	return raw, nil
}

// Parse decodes a SIGNAL frame body and its CallSignal without verifying the signature. The
// wrapper and the body must be canonical protobuf (services.md 1).
func Parse(raw []byte) (*Signal, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrMalformed)
	}
	var w pb.Signal
	if err := unmarshalCanonical(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: wrapper: %v", ErrMalformed, err)
	}
	if len(w.Body) == 0 {
		return nil, fmt.Errorf("%w: empty body", ErrMalformed)
	}
	var m pb.CallSignal
	if err := unmarshalCanonical(w.Body, &m); err != nil {
		return nil, fmt.Errorf("%w: body: %v", ErrMalformed, err)
	}
	return &Signal{Origin: w.Origin, Body: w.Body, Sig: w.Sig, Msg: &m}, nil
}

// Verify checks the action, the signature or the session binding, and the join payload. A
// forwarded signal is signed by its origin and its sender is the origin's device ID; a hop-local
// signal carries no signature and its sender is the authenticated session device.
func (s *Signal) Verify(sessionDevice [32]byte) error {
	if s == nil || s.Msg == nil {
		return fmt.Errorf("%w: nil", ErrMalformed)
	}
	m := s.Msg
	if _, ok := actionNames[m.Action]; !ok {
		return ErrUnknownAction
	}
	if len(m.Sender) != 32 {
		return ErrSpoofedSender
	}
	signed := len(s.Origin) > 0 || len(s.Sig) > 0
	if signed {
		if len(s.Origin) == 0 || len(s.Sig) == 0 {
			return ErrBadSignature
		}
		k, err := keys.ParseKind(s.Origin, keys.Ed25519)
		if err != nil {
			return ErrBadSignature
		}
		if err := keys.Verify(k, append([]byte(SignalLabel), s.Body...), s.Sig); err != nil {
			return ErrBadSignature
		}
		id := k.ID()
		if !bytes.Equal(m.Sender, id[:]) {
			return ErrSpoofedSender
		}
		if !IsForwarded(m.Action) {
			return ErrUnexpectedSignature
		}
	} else {
		if IsForwarded(m.Action) {
			return ErrMissingSignature
		}
		if !bytes.Equal(m.Sender, sessionDevice[:]) {
			return ErrSpoofedSender
		}
	}
	switch m.Action {
	case ActionJoin, ActionMemberHeartbeat:
		j := m.GetJoin()
		if j == nil {
			return ErrBadJoin
		}
		if err := ValidateJoin(j); err != nil {
			return err
		}
	}
	return nil
}

// ValidateJoin checks the per-join secrets of a join (services.md 9.1): a fresh Ed25519 media
// signing key (32 raw bytes) and a fresh 16-byte join_salt.
func ValidateJoin(j *pb.Join) error {
	if j == nil {
		return ErrBadJoin
	}
	if len(j.MediaKey) != ed25519PublicKeySize {
		return fmt.Errorf("%w: media key of %d bytes", ErrBadJoin, len(j.MediaKey))
	}
	if err := keys.CheckEd25519Public(j.MediaKey); err != nil {
		return fmt.Errorf("%w: media key: %v", ErrBadJoin, err)
	}
	if len(j.JoinSalt) != JoinSaltLen {
		return fmt.Errorf("%w: join_salt of %d bytes", ErrBadJoin, len(j.JoinSalt))
	}
	if len(j.Kx) != 0 && len(j.Kx) != 32 {
		return fmt.Errorf("%w: key exchange key of %d bytes", ErrBadJoin, len(j.Kx))
	}
	return nil
}

const ed25519PublicKeySize = 32

// NewJoin builds a join with a fresh per-join Ed25519 media signing key and a fresh 16-byte
// join_salt. The returned signer signs the member's media batches (services.md 9.3).
func NewJoin(profile *pb.PeerNodeProfile, proof, kx []byte) (*pb.Join, *keys.Signer, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, nil, err
	}
	signer, err := keys.NewEd25519Signer(seed)
	if err != nil {
		return nil, nil, err
	}
	salt := make([]byte, JoinSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, err
	}
	return &pb.Join{
		Profile:  profile,
		Proof:    append([]byte(nil), proof...),
		Kx:       append([]byte(nil), kx...),
		MediaKey: append([]byte(nil), signer.Public().Key()...),
		JoinSalt: salt,
	}, signer, nil
}

// marshalSignal encodes a Signal wrapper canonically.
func marshalSignal(s *pb.Signal) ([]byte, error) { return objects.Marshal(s) }

// unmarshalCanonical checks the canonical rules and decodes.
func unmarshalCanonical(b []byte, m proto.Message) error {
	if err := objects.CheckCanonical(b, m.ProtoReflect().Descriptor()); err != nil {
		return err
	}
	return proto.Unmarshal(b, m)
}
