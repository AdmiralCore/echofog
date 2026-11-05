package call

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

func testSigner(t testing.TB, seed byte) *keys.Signer {
	t.Helper()
	s, err := keys.NewEd25519Signer(bytes.Repeat([]byte{seed}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func deviceID(k keys.AKey) [32]byte { return k.ID() }

func deviceBytes(k keys.AKey) []byte {
	id := k.ID()
	return id[:]
}

func signFn(s *keys.Signer) func([]byte) ([]byte, error) {
	return func(b []byte) ([]byte, error) { return s.Sign(b), nil }
}

// v1Action is one action of call/signaling/signals.go.
type v1Action struct {
	name      string
	action    Action
	forwarded bool
	lossy     bool
}

var v1Actions = []v1Action{
	{"tree_manifest", ActionTreeManifest, true, false},
	{"coordinator_heartbeat", ActionCoordinatorHeartbeat, true, false},
	{"stream_announce", ActionStreamAnnounce, true, false},
	{"stream_close", ActionStreamClose, true, false},
	{"stream_tree_update", ActionStreamTreeUpdate, true, false},
	{"coordinator_election", ActionCoordinatorElection, true, false},
	{"vad_state", ActionVADState, true, false},
	{"join", ActionJoin, true, false},
	{"leave", ActionLeave, false, false},
	{"member_heartbeat", ActionMemberHeartbeat, false, false},
	{"parent_lost", ActionParentLost, false, false},
	{"stream_register", ActionStreamRegister, false, false},
	{"stream_unregister", ActionStreamUnregister, false, false},
	{"stream_subscribe", ActionStreamSubscribe, false, false},
	{"stream_unsubscribe", ActionStreamUnsubscribe, false, false},
	{"ping", ActionPing, false, false},
	{"sync_request", ActionSyncRequest, false, false},
	{"stream_reparent", ActionStreamReparent, false, false},
	{"report", ActionReport, false, true},
	{"pong", ActionPong, false, false},
	{"room_sync", ActionRoomSync, false, false},
	{"adopt", ActionAdopt, false, false},
	{"keyframe_request", ActionKeyframeRequest, false, false},
	{"kicked", ActionKicked, false, false},
	{"nack", ActionNack, false, true},
	{"stream_feedback", ActionStreamFeedback, false, true},
	{"stream_weak", ActionStreamWeak, false, true},
	{"media_key", ActionMediaKey, false, false},
	{"media_key_ack", ActionMediaKeyAck, false, false},
}

func TestActionNumbersAndNames(t *testing.T) {
	for i, a := range v1Actions {
		if got, want := int32(a.action), int32(i+1); got != want {
			t.Errorf("%s: action number %d, want %d", a.name, got, want)
		}
		if got := V1Name(a.action); got != a.name {
			t.Errorf("action %d: name %q, want %q", a.action, got, a.name)
		}
		if got, ok := ActionByName(a.name); !ok || got != a.action {
			t.Errorf("ActionByName(%q) = %d, %v", a.name, got, ok)
		}
		if got := IsForwarded(a.action); got != a.forwarded {
			t.Errorf("%s: IsForwarded = %v, want %v", a.name, got, a.forwarded)
		}
		if got := IsLossy(a.action); got != a.lossy {
			t.Errorf("%s: IsLossy = %v, want %v", a.name, got, a.lossy)
		}
	}
	if _, ok := ActionByName("future_action"); ok {
		t.Fatal("unknown name accepted")
	}
	if V1Name(Action(999)) != "" {
		t.Fatal("unknown action has a name")
	}
}

var testMediaKey = func() []byte {
	s, err := keys.NewEd25519Signer(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		panic(err)
	}
	return append([]byte(nil), s.Public().Key()...)
}()

func testPayload(a Action) *pb.CallSignal {
	m := &pb.CallSignal{
		Action:    a,
		CallId:    "room-1",
		MsgId:     7,
		Epoch:     3,
		Timestamp: 1000,
		Sender:    bytes.Repeat([]byte{9}, 32),
	}
	profile := &pb.PeerNodeProfile{PeerId: bytes.Repeat([]byte{1}, 32), UploadKbps: 500, RttNanos: 1000}
	switch a {
	case ActionTreeManifest:
		m.Payload = &pb.CallSignal_TreeManifest{TreeManifest: &pb.TreeManifest{
			CallId: "room-1", Epoch: 3, Coordinator: bytes.Repeat([]byte{1}, 32),
			Profiles: []*pb.ProfileEntry{{Peer: bytes.Repeat([]byte{1}, 32), Profile: profile}},
		}}
	case ActionCoordinatorHeartbeat:
		m.Payload = &pb.CallSignal_CoordinatorHeartbeat{CoordinatorHeartbeat: &pb.CoordinatorHeartbeat{
			Coordinator: bytes.Repeat([]byte{1}, 32), Epoch: 3, Members: 2,
			Streams: []*pb.StreamEntry{{StreamId: "s1", Epoch: 4}},
		}}
	case ActionStreamAnnounce, ActionStreamRegister:
		m.Payload = &pb.CallSignal_StreamAnnounce{StreamAnnounce: &pb.StreamAnnounce{
			StreamId: "s1", Broadcaster: bytes.Repeat([]byte{1}, 32), MediaType: "screen",
			BitrateKbps: 1000, Width: 1280, Height: 720, Fps: 30, Codec: "vp9", QualityId: "hd",
			Profile: profile,
		}}
	case ActionStreamClose, ActionStreamUnregister, ActionStreamUnsubscribe, ActionKeyframeRequest:
		m.Payload = &pb.CallSignal_StreamId{StreamId: "s1"}
	case ActionStreamTreeUpdate:
		m.Payload = &pb.CallSignal_StreamManifest{StreamManifest: &pb.StreamManifest{
			StreamId: "s1", Broadcaster: bytes.Repeat([]byte{1}, 32), MediaType: "camera",
			Epoch: 4, Subscribers: [][]byte{bytes.Repeat([]byte{2}, 32)},
		}}
	case ActionCoordinatorElection, ActionParentLost:
		m.Payload = &pb.CallSignal_Peer{Peer: bytes.Repeat([]byte{2}, 32)}
	case ActionVADState:
		m.Payload = &pb.CallSignal_Vad{Vad: &pb.VADState{
			Speaker: bytes.Repeat([]byte{1}, 32), Speaking: true, AudioDb: -12.5,
		}}
	case ActionJoin, ActionMemberHeartbeat:
		m.Payload = &pb.CallSignal_Join{Join: &pb.Join{
			Profile: profile, Proof: []byte{1, 2}, Kx: bytes.Repeat([]byte{3}, 32),
			MediaKey: append([]byte(nil), testMediaKey...), JoinSalt: bytes.Repeat([]byte{5}, 16),
		}}
	case ActionStreamSubscribe:
		m.Payload = &pb.CallSignal_StreamSubscribe{StreamSubscribe: &pb.StreamSubscribe{
			StreamId: "s1", Subscriber: bytes.Repeat([]byte{2}, 32), Profile: profile,
		}}
	case ActionStreamReparent:
		m.Payload = &pb.CallSignal_StreamReparent{StreamReparent: &pb.Reparent{
			StreamId: "s1", Avoid: bytes.Repeat([]byte{2}, 32), Reason: "silent",
		}}
	case ActionReport:
		m.Payload = &pb.CallSignal_Report{Report: &pb.Report{
			Target: bytes.Repeat([]byte{2}, 32), Reason: "spam",
		}}
	case ActionRoomSync:
		m.Payload = &pb.CallSignal_RoomSync{RoomSync: &pb.RoomSync{
			Voice: &pb.TreeManifest{CallId: "room-1", Epoch: 3, Coordinator: bytes.Repeat([]byte{1}, 32)},
		}}
	case ActionKicked:
		m.Payload = &pb.CallSignal_Reason{Reason: "reported for spam"}
	case ActionNack:
		m.Payload = &pb.CallSignal_Nack{Nack: &pb.Nack{
			StreamId: "s1", Lost: []*pb.NackEntry{{Seq: 7, Have: 3}},
		}}
	case ActionStreamFeedback:
		m.Payload = &pb.CallSignal_StreamFeedback{StreamFeedback: &pb.StreamFeedback{
			StreamId: "s1", Layered: true,
			Reports: []*pb.ViewerFeedback{{Viewer: bytes.Repeat([]byte{2}, 32), Level: 1, Kbps: 900, Loss: 0.1, DelayMs: 20}},
		}}
	case ActionStreamWeak:
		m.Payload = &pb.CallSignal_StreamWeak{StreamWeak: &pb.StreamWeak{
			StreamId: "s1", Weak: [][]byte{bytes.Repeat([]byte{2}, 32)},
		}}
	case ActionMediaKey:
		m.Payload = &pb.CallSignal_MediaKey{MediaKey: &pb.MediaKey{
			KeyEpoch: 4, Eph: bytes.Repeat([]byte{6}, 32), Sealed: []byte{7, 8},
			Signers: []*pb.SignerEntry{{Peer: bytes.Repeat([]byte{2}, 32), Key: bytes.Repeat([]byte{9}, 32)}},
		}}
	case ActionMediaKeyAck:
		m.Payload = &pb.CallSignal_MediaKeyAck{MediaKeyAck: &pb.KeyAck{Epoch: 4}}
	}
	return m
}

func TestEncodeParseVerifyAllActions(t *testing.T) {
	signer := testSigner(t, 1)
	self := deviceID(signer.Public())
	for _, a := range v1Actions {
		t.Run(a.name, func(t *testing.T) {
			m := testPayload(a.action)
			if a.forwarded {
				m.Sender = self[:]
			} else {
				m.Sender = self[:]
			}
			var origin keys.AKey
			var sign func([]byte) ([]byte, error)
			if a.forwarded {
				origin, sign = signer.Public(), signFn(signer)
			}
			raw, err := Encode(m, origin, sign)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			s, err := Parse(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := s.Verify(self); err != nil {
				t.Fatalf("verify: %v", err)
			}
			if !proto.Equal(s.Msg, m) {
				t.Fatalf("round trip differs:\n got %v\nwant %v", s.Msg, m)
			}
			if a.forwarded != (len(s.Origin) > 0) || a.forwarded != (len(s.Sig) > 0) {
				t.Fatalf("forwarded=%v origin=%d sig=%d", a.forwarded, len(s.Origin), len(s.Sig))
			}
		})
	}
}

func TestForwardedSignatureCoversExactBytes(t *testing.T) {
	signer := testSigner(t, 2)
	self := deviceID(signer.Public())
	m := testPayload(ActionTreeManifest)
	m.Sender = self[:]
	raw, err := Encode(m, signer.Public(), signFn(signer))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	// An unknown canonical field appended to the body changes the signed bytes but not the
	// decoded fields: the signature must be checked over the received bytes, not over a
	// re-encoding of the message.
	const unknownField = "\xf8\x7f\x01" // field 2047, varint 1
	body := append(append([]byte(nil), s.Body...), unknownField...)
	tampered, err := marshalSignal(&pb.Signal{Origin: s.Origin, Body: body, Sig: s.Sig})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := Parse(tampered)
	if err != nil {
		t.Fatalf("parse tampered: %v", err)
	}
	if err := ts.Verify(self); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("verify tampered body: %v, want ErrBadSignature", err)
	}
	// Signing the same exact bytes verifies: a relay never re-encodes them.
	signed := signer.Sign(append([]byte(SignalLabel), body...))
	resigned, err := marshalSignal(&pb.Signal{Origin: signer.Public(), Body: body, Sig: signed})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := Parse(resigned)
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Verify(self); err != nil {
		t.Fatalf("verify resigned body: %v", err)
	}
}

func TestTamperedForwardedSignalIsRejected(t *testing.T) {
	a := testSigner(t, 3)
	b := testSigner(t, 4)
	m := testPayload(ActionTreeManifest)
	m.Sender = deviceBytes(a.Public())
	raw, err := Encode(m, a.Public(), signFn(a))
	if err != nil {
		t.Fatal(err)
	}

	// Flip a byte of the call id inside the signed body; the protobuf stays valid.
	s, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte(nil), s.Body...)
	idx := bytes.Index(body, []byte("room-1"))
	if idx < 0 {
		t.Fatal("call id not found in body")
	}
	body[idx] = 'R'
	tampered, err := marshalSignal(&pb.Signal{Origin: s.Origin, Body: body, Sig: s.Sig})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := Parse(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Verify([32]byte{}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body: %v, want ErrBadSignature", err)
	}

	// Replacing the origin with another key breaks the signature too.
	forged, err := marshalSignal(&pb.Signal{Origin: b.Public(), Body: s.Body, Sig: s.Sig})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := Parse(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Verify([32]byte{}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("forged origin: %v, want ErrBadSignature", err)
	}

	// A signature by another key over the same body, with the sender changed to that key, is a
	// different (valid) originator; the service layer checks it against the manifest.
	m.Sender = deviceBytes(b.Public())
	other, err := Encode(m, b.Public(), signFn(b))
	if err != nil {
		t.Fatal(err)
	}
	os, err := Parse(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Verify([32]byte{}); err != nil {
		t.Fatalf("valid signal by another origin: %v", err)
	}
}

func TestHopLocalSignalsCarryNoSignature(t *testing.T) {
	signer := testSigner(t, 5)
	self := deviceID(signer.Public())
	m := testPayload(ActionPing)
	m.Sender = self[:]
	raw, err := Encode(m, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(self); err != nil {
		t.Fatalf("hop-local verify: %v", err)
	}
	if err := s.Verify(deviceID(testSigner(t, 6).Public())); !errors.Is(err, ErrSpoofedSender) {
		t.Fatalf("wrong session peer: %v, want ErrSpoofedSender", err)
	}
	// A forwarded action without a signature is rejected.
	fm := testPayload(ActionStreamAnnounce)
	fm.Sender = self[:]
	fraw, err := Encode(fm, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := Parse(fraw)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Verify(self); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("unsigned forwarded action: %v, want ErrMissingSignature", err)
	}
	// A hop-local action with a signature is rejected: hop-local signals carry none.
	sm := testPayload(ActionPing)
	sm.Sender = self[:]
	sraw, err := Encode(sm, signer.Public(), signFn(signer))
	if err != nil {
		t.Fatal(err)
	}
	ss, err := Parse(sraw)
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.Verify(self); !errors.Is(err, ErrUnexpectedSignature) {
		t.Fatalf("signed hop-local action: %v, want ErrUnexpectedSignature", err)
	}
}

func TestUnknownActionIsRejected(t *testing.T) {
	signer := testSigner(t, 7)
	m := testPayload(ActionPing)
	m.Action = Action(999)
	m.Sender = deviceBytes(signer.Public())
	raw, err := Encode(m, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(deviceID(signer.Public())); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("unknown action: %v, want ErrUnknownAction", err)
	}
}

func TestJoinCarriesFreshKeyAndSalt(t *testing.T) {
	profile := &pb.PeerNodeProfile{PeerId: bytes.Repeat([]byte{1}, 32)}
	j1, signer1, err := NewJoin(profile, []byte("proof"), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	j2, signer2, err := NewJoin(profile, []byte("proof"), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if len(j1.MediaKey) != 32 || len(j1.JoinSalt) != 16 {
		t.Fatalf("media key %d, salt %d", len(j1.MediaKey), len(j1.JoinSalt))
	}
	if bytes.Equal(j1.MediaKey, j2.MediaKey) || bytes.Equal(j1.JoinSalt, j2.JoinSalt) {
		t.Fatal("two joins reused a media key or a salt")
	}
	if bytes.Equal(signer1.Public().Key(), signer2.Public().Key()) {
		t.Fatal("two joins reused a media signer")
	}
	if err := ValidateJoin(j1); err != nil {
		t.Fatalf("validate join: %v", err)
	}

	bad := proto.Clone(j1).(*pb.Join)
	bad.JoinSalt = []byte{1, 2, 3}
	if err := ValidateJoin(bad); !errors.Is(err, ErrBadJoin) {
		t.Fatalf("short salt: %v, want ErrBadJoin", err)
	}
	bad = proto.Clone(j1).(*pb.Join)
	bad.MediaKey = make([]byte, 32) // not a canonical Ed25519 point
	if err := ValidateJoin(bad); !errors.Is(err, ErrBadJoin) {
		t.Fatalf("bad media key: %v, want ErrBadJoin", err)
	}
	bad = proto.Clone(j1).(*pb.Join)
	bad.MediaKey = bad.MediaKey[:31]
	if err := ValidateJoin(bad); !errors.Is(err, ErrBadJoin) {
		t.Fatalf("short media key: %v, want ErrBadJoin", err)
	}
	// A join signal with a malformed join payload is dropped even when signed.
	msg := testPayload(ActionJoin)
	signer := testSigner(t, 8)
	msg.Sender = deviceBytes(signer.Public())
	msg.GetJoin().JoinSalt = []byte{1, 2, 3}
	raw, err := Encode(msg, signer.Public(), signFn(signer))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify([32]byte{}); !errors.Is(err, ErrBadJoin) {
		t.Fatalf("join validation: %v, want ErrBadJoin", err)
	}
}

func TestParseRejectsNonCanonical(t *testing.T) {
	// origin encoded as an explicit empty value (default must be omitted).
	raw := []byte{0x0a, 0x00, 0x12, 0x02, 0x08, 0x01}
	if _, err := Parse(raw); !errors.Is(err, ErrMalformed) {
		t.Fatalf("non-canonical wrapper: %v, want ErrMalformed", err)
	}
	// body with a field out of order is not canonical.
	m := testPayload(ActionPing)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = marshalSignal(&pb.Signal{Body: append(append([]byte(nil), b...), 0x10, 0x01)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); !errors.Is(err, ErrMalformed) {
		t.Fatalf("non-canonical body: %v, want ErrMalformed", err)
	}
}

func TestJoinVector(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "testdata", "kdf.json"))
	if err != nil {
		t.Fatalf("read kdf.json: %v", err)
	}
	var v struct {
		Call2 struct {
			JoinSalt    string `json:"join_salt"`
			MediaKeyPub string `json:"media_key_pub"`
		} `json:"call2"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	salt, err := hex.DecodeString(v.Call2.JoinSalt)
	if err != nil {
		t.Fatal(err)
	}
	key, err := hex.DecodeString(v.Call2.MediaKeyPub)
	if err != nil {
		t.Fatal(err)
	}
	if len(salt) != 16 || len(key) != 32 {
		t.Fatalf("vector lengths: salt %d, media key %d", len(salt), len(key))
	}
	j := &pb.Join{MediaKey: key, JoinSalt: salt}
	if err := ValidateJoin(j); err != nil {
		t.Fatalf("vector join: %v", err)
	}
}

func FuzzParse(f *testing.F) {
	signer := testSigner(f, 9)
	m := testPayload(ActionTreeManifest)
	m.Sender = deviceBytes(signer.Public())
	raw, _ := Encode(m, signer.Public(), signFn(signer))
	f.Add(raw)
	ping, _ := Encode(testPayload(ActionPing), nil, nil)
	f.Add(ping)
	f.Add([]byte{})
	f.Add([]byte{0x0a})
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := Parse(b)
		if err != nil {
			return
		}
		_ = s.Verify([32]byte{})
	})
}
