package service

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// The upgradability test (versioning.md §2–§3): a "v2.1" node adds
//   - a new service (names/2, service 9, reserved in v2.0),
//   - a new chat/2 frame type that peers may ignore (0x44 REACTION, sent with IGNORABLE),
//   - a new feature (0x0013),
//   - a new protobuf field (DELIVER.priority = 4, DELIVERED.read = 3),
//   - a new IGNORABLE session-level control frame (0x15) and a new datagram type (0x14),
// and must interoperate with a v2.0 node in both roles, with no abuse score, while two v2.1
// nodes use the new things.

const (
	featureV21  uint16 = 0x0013
	typeReact   uint64 = 0x44
	ctrlV21     uint64 = 0x15
	dgramV21    uint64 = 0x14
	statusOK           = 0
	fieldReq           = 1
	fieldEnv           = 2
	fieldStatus        = 2
	fieldPrio          = 4 // new in v2.1 (DELIVER)
	fieldRead          = 3 // new in v2.1 (DELIVERED)
)

var chatV21 = Spec{ID: Chat, Name: "chat", Versions: map[uint64]Version{2: {Types: append(
	slices.Clone(builtin[Chat].Versions[2].Types), FrameType{typeReact, "REACTION", 256, false})}}}

var namesV2 = Spec{ID: Names, Name: "names", Versions: map[uint64]Version{2: {Types: []FrameType{
	{0x40, "RESOLVE", 128, false}, {0x41, "BINDING", 1024, false}}}}}

// pbFields decodes a protobuf message as v2.0 code does: known varint fields by number, every
// other field skipped (versioning.md §3.5 rule 3).
func pbFields(t testing.TB, b []byte) map[protowire.Number]uint64 {
	out := map[protowire.Number]uint64{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			t.Errorf("bad protobuf tag in %x", b)
			return out
		}
		b = b[n:]
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			t.Errorf("bad protobuf value in %x", b)
			return out
		}
		if typ == protowire.VarintType {
			v, _ := protowire.ConsumeVarint(b)
			out[num] = v
		}
		b = b[n:]
	}
	return out
}

func deliverMsg(req uint64, v21 bool) []byte {
	b := protowire.AppendTag(nil, fieldReq, protowire.VarintType)
	b = protowire.AppendVarint(b, req)
	b = protowire.AppendTag(b, fieldEnv, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte("sealed envelope"))
	if v21 {
		b = protowire.AppendTag(b, fieldPrio, protowire.VarintType)
		b = protowire.AppendVarint(b, 7)
	}
	return b
}

func deliveredMsg(req uint64, v21 bool) []byte {
	b := protowire.AppendTag(nil, fieldReq, protowire.VarintType)
	b = protowire.AppendVarint(b, req)
	if v21 {
		// status 0 is the default and omitted; the new field follows.
		b = protowire.AppendTag(b, fieldRead, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	return b
}

// chatHandler answers each DELIVER with DELIVERED; a v2.1 handler also sends a REACTION first.
func chatHandler(t testing.TB, v21 bool, reactions *atomic.Int32) Handler {
	return func(ctx context.Context, st *mux.Stream) {
		defer st.Close()
		for {
			f, err := st.ReadFrame()
			if err != nil {
				return
			}
			switch f.Type {
			case 0x40:
				m := pbFields(t, f.Payload)
				if v21 {
					st.WriteFrame(typeReact, frame.FlagIgnorable, []byte("👍"))
				}
				st.WriteFrame(0x41, 0, deliveredMsg(m[fieldReq], v21))
			case typeReact:
				reactions.Add(1)
			}
		}
	}
}

func newNode(t testing.TB, v21 bool, reactions *atomic.Int32) *peer {
	r := NewRegistry()
	if v21 {
		r.ServeSpec(chatV21, 2, chatHandler(t, true, reactions))
		r.ServeSpec(namesV2, 2, func(ctx context.Context, st *mux.Stream) {
			defer st.Close()
			if f, err := st.ReadFrame(); err == nil && f.Type == 0x40 {
				st.WriteFrame(0x41, 0, []byte("binding"))
			}
		})
		return &peer{reg: r, features: []uint16{FeaturePathMigration, featureV21}}
	}
	r.Serve(Chat, 2, chatHandler(t, false, reactions))
	return &peer{reg: r, features: []uint16{FeaturePathMigration}}
}

// talkChat sends a DELIVER (v2.1 form when v21) and returns the DELIVERED fields and how many
// REACTION frames the client saw.
func talkChat(t *testing.T, s *mux.Session, p *Policy, v21 bool) (map[protowire.Number]uint64, int) {
	t.Helper()
	st, err := Open(ctxT(t), s, p, Chat)
	if err != nil {
		t.Fatal(err)
	}
	if v21 {
		if err := st.WriteFrame(typeReact, frame.FlagIgnorable, []byte("hello from 2.1")); err != nil {
			t.Fatal(err)
		}
	}
	st.WriteFrame(0x40, 0, deliverMsg(42, v21))
	reacts := 0
	for {
		f, err := st.ReadFrame()
		if err != nil {
			t.Fatalf("chat/2 broke: %v", err)
		}
		switch f.Type {
		case typeReact:
			reacts++
		case 0x41:
			st.Close()
			return pbFields(t, f.Payload), reacts
		default:
			t.Fatalf("unexpected frame %#x", f.Type)
		}
	}
}

func checkHealthy(t *testing.T, l *link, nodes ...*peer) {
	t.Helper()
	for _, n := range nodes {
		if n.abuse.Load() != 0 {
			t.Fatal("a v2.1 feature raised an abuse score")
		}
	}
	for _, s := range []*mux.Session{l.a, l.b} {
		if _, err := s.Ping(ctxT(t)); err != nil {
			t.Fatalf("session broken: %v", err)
		}
	}
}

func TestUpgradeV21InitiatorWithV20Responder(t *testing.T) {
	var r20, r21 atomic.Int32
	old, nw := newNode(t, false, &r20), newNode(t, true, &r21)
	l := connect(t, nw, old)

	// Negotiation: the new service and the new feature are simply not selected.
	if _, ok := l.polA.Selected(Names); ok {
		t.Fatal("names/2 selected by a v2.0 responder")
	}
	if v, ok := l.polA.Selected(Chat); !ok || v != 2 {
		t.Fatal("chat/2 not selected")
	}
	if l.a.HasFeature(featureV21) || !l.a.HasFeature(FeaturePathMigration) {
		t.Fatal("feature negotiation")
	}
	// The v2.1 node falls back when its new service is not selected ...
	if _, err := Open(ctxT(t), l.a, l.polA, Names); !errors.Is(err, ErrNotSelected) {
		t.Fatalf("%v", err)
	}
	// ... and if it opens it anyway, it gets a polite UNSUPPORTED{names, 0, 0}.
	st, _ := l.a.OpenStream(ctxT(t), Names, 2)
	st.WriteFrame(0x40, 0, []byte("alice.ton"))
	_, err := st.ReadFrame()
	var ue *mux.UnsupportedError
	if !errors.As(err, &ue) || ue.Service != Names || ue.Min != 0 || ue.Max != 0 {
		t.Fatalf("%v", err)
	}
	// chat/2 with the new frame type and the new protobuf field: the v2.0 node skips both.
	got, reacts := talkChat(t, l.a, l.polA, true)
	if got[fieldReq] != 42 || reacts != 0 || r20.Load() != 0 {
		t.Fatalf("DELIVERED %v reacts %d", got, reacts)
	}
	// New control frame, GREASE and a new datagram type: skipped and counted.
	if err := l.a.SendControl(ctrlV21, frame.FlagIgnorable, []byte("2.1 control")); err != nil {
		t.Fatal(err)
	}
	l.a.SendGrease()
	l.a.SendDatagram(dgramV21, []byte("2.1 datagram"))
	l.a.SendDatagram(frame.DgramCallAudio, []byte("2.0 datagram"))
	if d, err := l.b.ReceiveDatagram(ctxT(t)); err != nil || d.Type != frame.DgramCallAudio {
		t.Fatalf("%+v %v", d, err)
	}
	if l.b.Stats().DatagramsUnknown != 1 {
		t.Fatalf("%+v", l.b.Stats())
	}
	checkHealthy(t, l, old, nw)
}

func TestUpgradeV20InitiatorWithV21Responder(t *testing.T) {
	var r20, r21 atomic.Int32
	old, nw := newNode(t, false, &r20), newNode(t, true, &r21)
	l := connect(t, old, nw)
	if l.a.HasFeature(featureV21) {
		t.Fatal("v2.1 feature selected with a v2.0 initiator")
	}
	// The v2.1 responder answers with a REACTION frame and a DELIVERED with a new field; the
	// v2.0 client skips the frame (IGNORABLE, unknown to its chat/2 limiter) and the field.
	got, reacts := talkChat(t, l.a, l.polA, false)
	if got[fieldReq] != 42 || got[fieldStatus] != statusOK || reacts != 0 {
		t.Fatalf("DELIVERED %v reacts %d", got, reacts)
	}
	// The v2.1 responder may also open streams to the v2.0 node over the selected pair.
	st, err := Open(ctxT(t), l.b, l.polB, Chat)
	if err != nil {
		t.Fatal(err)
	}
	st.WriteFrame(typeReact, frame.FlagIgnorable, []byte("hi"))
	st.WriteFrame(0x40, 0, deliverMsg(7, true))
	for {
		f, err := st.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == 0x41 {
			if pbFields(t, f.Payload)[fieldReq] != 7 {
				t.Fatal("req not echoed")
			}
			break
		}
	}
	checkHealthy(t, l, old, nw)
}

func TestUpgradeV21BothSidesUseTheNewThings(t *testing.T) {
	var ra, rb atomic.Int32
	a, b := newNode(t, true, &ra), newNode(t, true, &rb)
	l := connect(t, a, b)
	if !l.a.HasFeature(featureV21) {
		t.Fatal("new feature not selected between two v2.1 nodes")
	}
	if v, ok := l.polA.Selected(Names); !ok || v != 2 {
		t.Fatal("names/2 not selected between two v2.1 nodes")
	}
	st, err := Open(ctxT(t), l.a, l.polA, Names)
	if err != nil {
		t.Fatal(err)
	}
	st.WriteFrame(0x40, 0, []byte("alice.ton"))
	if f, err := st.ReadFrame(); err != nil || string(f.Payload) != "binding" {
		t.Fatalf("%v", err)
	}
	got, reacts := talkChat(t, l.a, l.polA, true)
	if got[fieldRead] != 1 || reacts != 1 {
		t.Fatalf("DELIVERED %v reacts %d", got, reacts)
	}
	deadline := time.Now().Add(2 * time.Second)
	for rb.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	checkHealthy(t, l, a, b)
}
