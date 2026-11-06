package v1sig

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/internal/v2/call"
	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func pid(b byte) types.PeerID { return types.PeerID{b, b, b} }

func prof(b byte) tree.PeerNodeProfile {
	return tree.PeerNodeProfile{
		PeerID: pid(b), UploadKbps: 4000, MaxRelayChildren: 3, RTT: 40 * time.Millisecond,
		PacketLossPercent: 1.5, IsMobile: true, RelayOnly: true,
		LastHeartbeat: time.Unix(1700000000, 12345), Backdrop: "waves", DisplayHz: 144,
		SignalKX: bytes.Repeat([]byte{b}, 32),
	}
}

func voice() *tree.TreeManifest {
	return &tree.TreeManifest{
		CallID: "c1", Epoch: 7, Coordinator: pid(1), StandbyCoordinators: []types.PeerID{pid(2), pid(3)},
		ParentMap:   map[types.PeerID]tree.ParentAssignment{pid(2): {PrimaryParent: pid(1), StandbyParent: pid(3)}, pid(3): {PrimaryParent: pid(1)}},
		ChildrenMap: map[types.PeerID][]types.PeerID{pid(1): {pid(2), pid(3)}},
		Profiles:    map[types.PeerID]tree.PeerNodeProfile{pid(2): prof(2), pid(3): prof(3)},
		Timestamp:   1700000000123,
	}
}

func streamMF() *tree.StreamManifest {
	return &tree.StreamManifest{
		StreamID: "s", Broadcaster: pid(2), MediaType: "screen", BitrateKbps: 6000, Width: 1920, Height: 1080, FPS: 60,
		Codec: "h264", QualityID: "1080p60", Epoch: 3, Subscribers: []types.PeerID{pid(3), pid(4)},
		DistributionTree: map[types.PeerID][]types.PeerID{pid(2): {pid(3)}, pid(3): {pid(4)}},
		ParentMap:        map[types.PeerID]types.PeerID{pid(3): pid(2), pid(4): pid(3)},
		Profiles:         map[types.PeerID]tree.PeerNodeProfile{pid(2): prof(2)},
		Overflow:         []types.PeerID{pid(5)}, Avoid: map[types.PeerID][]types.PeerID{pid(4): {pid(3)}}, Weak: []types.PeerID{pid(4)},
	}
}

func announce() signaling.StreamAnnouncePayload {
	return signaling.StreamAnnouncePayload{StreamID: "s", Broadcaster: pid(2), MediaType: "camera", BitrateKbps: 2000,
		Width: 1280, Height: 720, FPS: 30, Codec: "vp8", QualityID: "720p30", Profile: prof(2)}
}

// cases: every v1 action with a payload of the type the call layer sends.
var cases = []struct {
	action  signaling.SignalAction
	payload any
	fresh   func() any // the typed zero value to decode into for comparison
}{
	{signaling.ActionTreeManifest, voice(), func() any { return new(tree.TreeManifest) }},
	{signaling.ActionCoordinatorHeartbeat, signaling.CoordinatorHeartbeatPayload{Coordinator: pid(1), Epoch: 7, Streams: map[string]uint64{"s": 3, "t": 1}, Members: 5}, func() any { return new(signaling.CoordinatorHeartbeatPayload) }},
	{signaling.ActionStreamAnnounce, announce(), func() any { return new(signaling.StreamAnnouncePayload) }},
	{signaling.ActionStreamClose, "s", func() any { return new(string) }},
	{signaling.ActionStreamTreeUpdate, streamMF(), func() any { return new(tree.StreamManifest) }},
	{signaling.ActionCoordinatorElection, pid(4), func() any { return new(types.PeerID) }},
	{signaling.ActionVADState, signaling.VADPayload{Speaker: pid(2), Speaking: true, AudioDb: -23.5}, func() any { return new(signaling.VADPayload) }},
	{signaling.ActionJoin, signaling.JoinPayload{Profile: prof(2), Proof: bytes.Repeat([]byte{9}, 32), KX: bytes.Repeat([]byte{8}, 32), SK: bytes.Repeat([]byte{7}, 32), JoinSalt: bytes.Repeat([]byte{6}, 16)}, func() any { return new(signaling.JoinPayload) }},
	{signaling.ActionLeave, nil, nil},
	{signaling.ActionMemberHeartbeat, signaling.JoinPayload{Profile: prof(3), Proof: bytes.Repeat([]byte{9}, 32), KX: bytes.Repeat([]byte{8}, 32), SK: bytes.Repeat([]byte{7}, 32), JoinSalt: bytes.Repeat([]byte{6}, 16)}, func() any { return new(signaling.JoinPayload) }},
	{signaling.ActionParentLost, pid(4), func() any { return new(types.PeerID) }},
	{signaling.ActionStreamRegister, announce(), func() any { return new(signaling.StreamAnnouncePayload) }},
	{signaling.ActionStreamUnregister, "s", func() any { return new(string) }},
	{signaling.ActionStreamSubscribe, signaling.StreamSubscribePayload{StreamID: "s", Subscriber: pid(3), Profile: prof(3)}, func() any { return new(signaling.StreamSubscribePayload) }},
	{signaling.ActionStreamUnsub, "s", func() any { return new(string) }},
	{signaling.ActionPing, nil, nil},
	{signaling.ActionSyncRequest, nil, nil},
	{signaling.ActionStreamReparent, signaling.ReparentPayload{StreamID: "s", Avoid: pid(3), Reason: "silent"}, func() any { return new(signaling.ReparentPayload) }},
	{signaling.ActionReport, signaling.ReportPayload{Target: pid(4), Reason: "spam"}, func() any { return new(signaling.ReportPayload) }},
	{signaling.ActionPong, nil, nil},
	{signaling.ActionRoomSync, signaling.RoomSyncPayload{Voice: voice(), Streams: []*tree.StreamManifest{streamMF()}, Announces: []signaling.StreamAnnouncePayload{announce()}}, func() any { return new(signaling.RoomSyncPayload) }},
	{signaling.ActionAdopt, nil, nil},
	{signaling.ActionKeyframeRequest, "s", func() any { return new(string) }},
	{signaling.ActionKicked, "spam", func() any { return new(string) }},
	{signaling.ActionNack, signaling.NackPayload{StreamID: "s", Lost: []media.NackEntry{{Seq: 10, Have: 3}, {Seq: 12}}}, func() any { return new(signaling.NackPayload) }},
	{signaling.ActionStreamFeedback, signaling.StreamFeedbackPayload{StreamID: "s", Reports: []signaling.ViewerFeedback{{Viewer: pid(3), Level: 2, Kbps: 900, Loss: 0.25, DelayMs: 40}}, Layered: true}, func() any { return new(signaling.StreamFeedbackPayload) }},
	{signaling.ActionStreamWeak, signaling.StreamWeakPayload{StreamID: "s", Weak: []types.PeerID{pid(4), pid(5)}}, func() any { return new(signaling.StreamWeakPayload) }},
	{signaling.ActionMediaKey, signaling.MediaKeyPayload{KeyEpoch: 4, Eph: bytes.Repeat([]byte{1}, 32), Sealed: bytes.Repeat([]byte{2}, 60), Signers: map[types.PeerID][]byte{pid(2): bytes.Repeat([]byte{3}, 32), pid(3): bytes.Repeat([]byte{4}, 32)}}, func() any { return new(signaling.MediaKeyPayload) }},
	{signaling.ActionMediaKeyAck, signaling.KeyAckPayload{Epoch: 4}, func() any { return new(signaling.KeyAckPayload) }},
}

func envelope(t *testing.T, action signaling.SignalAction, payload any) []byte {
	t.Helper()
	b, err := signaling.EncodeSignal(action, "c1", pid(2), 7, payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func norm(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every action survives v1 -> CallSignal -> canonical bytes -> CallSignal -> v1.
func TestRoundTripAllActions(t *testing.T) {
	if len(cases) != 29 {
		t.Fatalf("%d cases, the v1 call has 29 actions", len(cases))
	}
	for _, c := range cases {
		t.Run(string(c.action), func(t *testing.T) {
			orig := envelope(t, c.action, c.payload)
			m, err := ToPB(orig)
			if err != nil {
				t.Fatalf("ToPB: %v", err)
			}
			if v1name := call.V1Name(m.Action); v1name != string(c.action) {
				t.Fatalf("action %v maps to %q", m.Action, v1name)
			}
			if string(m.Sender) != string(pid(2)[:]) || m.CallId != "c1" || m.Epoch != 7 {
				t.Fatalf("header: %+v", m)
			}
			// Through the wire form: canonical protobuf, as call.Encode / call.Parse do.
			raw, err := call.Encode(m, nil, nil)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			parsed, err := call.Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			back, err := ToV1(parsed.Msg)
			if err != nil {
				t.Fatalf("ToV1: %v", err)
			}
			want, err := signaling.DecodeSignal(orig)
			if err != nil {
				t.Fatal(err)
			}
			got, err := signaling.DecodeSignal(back)
			if err != nil {
				t.Fatalf("DecodeSignal: %v", err)
			}
			if got.Action != want.Action || got.CallID != want.CallID || got.Sender != want.Sender ||
				got.MsgID != want.MsgID || got.Epoch != want.Epoch || got.Timestamp != want.Timestamp {
				t.Fatalf("header differs:\n got %+v\nwant %+v", got, want)
			}
			if len(got.Signature) != 0 {
				t.Fatal("the mapped envelope must be unsigned: call/2 authenticates it")
			}
			if c.fresh == nil {
				if len(got.Payload) != 0 && string(got.Payload) != "null" {
					t.Fatalf("payload %s, want none", got.Payload)
				}
				return
			}
			a, b := c.fresh(), c.fresh()
			if err := json.Unmarshal(want.Payload, a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got.Payload, b); err != nil {
				t.Fatalf("payload %s: %v", got.Payload, err)
			}
			if norm(t, a) != norm(t, b) {
				t.Fatalf("payload differs:\n got %s\nwant %s", norm(t, b), norm(t, a))
			}
		})
	}
}

// The bytes of a mapped signal are deterministic, so a signature over them is stable.
func TestToPBIsDeterministic(t *testing.T) {
	orig := envelope(t, signaling.ActionTreeManifest, voice())
	var first []byte
	for i := 0; i < 20; i++ {
		m, err := ToPB(orig)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := call.Encode(m, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = raw
		} else if !bytes.Equal(first, raw) {
			t.Fatal("two encodings of one manifest differ")
		}
	}
}

func TestToPBRejects(t *testing.T) {
	good := func(a signaling.SignalAction, p any) []byte { return envelope(t, a, p) }
	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"not json", []byte("{")},
		{"unknown action", good("teleport", nil)},
		{"payload of the wrong type", good(signaling.ActionTreeManifest, "text")},
		{"sender not 32 bytes", []byte(`{"action":"ping","call_id":"c","sender":"AAAA"}`)},
		{"payload missing", good(signaling.ActionTreeManifest, nil)},
		{"join without media key", good(signaling.ActionJoin, signaling.JoinPayload{Profile: prof(2), Proof: []byte{1}, KX: bytes.Repeat([]byte{8}, 32)})},
		{"peer of the wrong length", good(signaling.ActionParentLost, "not-a-peer")},
	}
	for _, c := range cases {
		if m, err := ToPB(c.in); err == nil {
			t.Errorf("%s: accepted: %+v", c.name, m)
		}
	}
}

func TestToV1Rejects(t *testing.T) {
	bad := []*pb.CallSignal{
		nil,
		{Action: pb.Action_ACTION_UNSPECIFIED, Sender: bytes.Repeat([]byte{1}, 32)},
		{Action: pb.Action_ACTION_PING, Sender: []byte{1}},
		{Action: pb.Action_ACTION_TREE_MANIFEST, Sender: bytes.Repeat([]byte{1}, 32)},                                             // no payload
		{Action: pb.Action_ACTION_STREAM_CLOSE, Sender: bytes.Repeat([]byte{1}, 32), Payload: &pb.CallSignal_Reason{Reason: "x"}}, // wrong oneof
		{Action: pb.Action(99), Sender: bytes.Repeat([]byte{1}, 32)},
	}
	for i, m := range bad {
		if b, err := ToV1(m); err == nil {
			t.Errorf("case %d accepted: %s", i, b)
		} else if !errors.Is(err, ErrMapping) {
			t.Errorf("case %d: %v is not ErrMapping", i, err)
		}
	}
}

func FuzzRoundTrip(f *testing.F) {
	for _, c := range cases {
		b, _ := signaling.EncodeSignal(c.action, "c1", pid(2), 7, c.payload)
		f.Add(b)
	}
	f.Add([]byte(`{"action":"join","payload":"AAAA"}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		m, err := ToPB(in)
		if err != nil {
			return
		}
		raw, err := call.Encode(m, nil, nil)
		if err != nil {
			return
		}
		p, err := call.Parse(raw)
		if err != nil {
			return
		}
		if _, err := ToV1(p.Msg); err != nil {
			t.Fatalf("a mapped signal does not map back: %v", err)
		}
	})
}
