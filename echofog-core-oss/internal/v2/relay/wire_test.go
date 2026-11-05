package relay

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

func id(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestFrameRoundTrips(t *testing.T) {
	c := Connect{Req: 7, Target: id(1), Mode: ModeDatagram, Token: []byte("t"), TargetKind: TargetRendezvous, WantAddr: true}
	if got, err := ParseConnect(c.Append(nil)); err != nil || !bytes.Equal(got.Target, c.Target) || got.Mode != 2 || !got.WantAddr || got.TargetKind != 1 || got.Req != 7 {
		t.Fatalf("connect: %+v %v", got, err)
	}
	in := Incoming{CID: 9, Source: id(2), Mode: 1, Target: id(3), WantAddr: true}
	if got, err := ParseIncoming(in.Append(nil)); err != nil || got.CID != 9 || !bytes.Equal(got.Source, in.Source) || !got.WantAddr {
		t.Fatalf("incoming: %+v %v", got, err)
	}
	if got, err := ParseAccept(Accept{CID: 5, WantAddr: true}.Append(nil)); err != nil || got.CID != 5 || !got.WantAddr {
		t.Fatalf("accept: %+v %v", got, err)
	}
	if got, err := ParseRefused(Refused{Req: 1, Target: id(4), Reason: 2}.Append(nil)); err != nil || got.Reason != 2 {
		t.Fatalf("refused: %+v %v", got, err)
	}
	r := Register{Req: 3, Day: 20760}
	for i := 0; i < MaxRegisterIDs; i++ {
		r.IDs = append(r.IDs, id(byte(i)))
	}
	if got, err := ParseRegister(r.Append(nil)); err != nil || len(got.IDs) != MaxRegisterIDs || got.Day != 20760 {
		t.Fatalf("register: %d %v", len(got.IDs), err)
	}
}

func TestMalformedFrames(t *testing.T) {
	over := Register{IDs: make([][]byte, MaxRegisterIDs+1)}
	for i := range over.IDs {
		over.IDs[i] = id(1)
	}
	cases := map[string]func() error{
		"connect without target":  func() error { _, e := ParseConnect(Connect{Req: 1, Mode: 1}.Append(nil)); return e },
		"connect short target":    func() error { _, e := ParseConnect([]byte{0x12, 0x02, 1, 2}); return e },
		"connect wire type":       func() error { _, e := ParseConnect([]byte{0x08, 0x01, 0x12}); return e },
		"fields out of order":     func() error { _, e := ParseAccept([]byte{0x10, 0x01, 0x08, 0x01}); return e },
		"duplicate field":         func() error { _, e := ParseAccept([]byte{0x08, 0x01, 0x08, 0x01}); return e },
		"bool above 1":            func() error { _, e := ParseAccept([]byte{0x10, 0x02}); return e },
		"mode above uint32":       func() error { _, e := ParseConnect([]byte{0x18, 0xff, 0xff, 0xff, 0xff, 0x1f}); return e },
		"register over 256 ids":   func() error { _, e := ParseRegister(over.Append(nil)); return e },
		"register short id":       func() error { _, e := ParseRegister([]byte{0x12, 0x01, 0x01}); return e },
		"peer_addr too long":      func() error { _, e := ParsePeerAddr(PeerAddr{CID: 1, Addr: make([]byte, 20)}.Append(nil)); return e },
		"truncated varint":        func() error { _, e := ParseRefuse([]byte{0x08, 0x80}); return e },
		"incoming target is a id": func() error { _, e := ParseIncoming([]byte{0x22, 0x01, 0x01}); return e },
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}
	// Unknown fields are skipped (versioning.md §3.5).
	b := append(Accept{CID: 5}.Append(nil), 0x38, 0x01)
	if got, err := ParseAccept(b); err != nil || got.CID != 5 {
		t.Fatalf("unknown field: %+v %v", got, err)
	}
}

// frames.json RELAY_DGRAM: payload = varint circuit_id ‖ datagram.
func TestRelayDgramVector(t *testing.T) {
	payload, _ := hex.DecodeString("404d7365616c6564e280a6")
	cid, n, err := frame.ParseVarint(payload)
	if err != nil || cid != 77 || string(payload[n:]) != "sealed…" {
		t.Fatalf("cid %d rest %q err %v", cid, payload[n:], err)
	}
	h := NewHub()
	var sent []byte
	d := h.newCircuit(77, func(typ uint64, p []byte) error {
		if typ != frame.DgramRelay {
			t.Fatalf("type %#x", typ)
		}
		sent = p
		return nil
	}, nil)
	if err := d.Send([]byte("sealed…")); err != nil || !bytes.Equal(sent, payload) {
		t.Fatalf("sent %x err %v", sent, err)
	}
	h.HandleDatagram(payload)
	h.HandleDatagram(append(frame.AppendVarint(nil, 78), 1)) // unknown circuit
	if h.Dropped() != 1 {
		t.Fatalf("dropped %d, want 1", h.Dropped())
	}
}

func FuzzParse(f *testing.F) {
	f.Add(Connect{Req: 1, Target: id(1), Mode: 1}.Append(nil))
	f.Add(Register{IDs: [][]byte{id(1), id(2)}}.Append(nil))
	f.Add(Incoming{CID: 1, Source: id(1), Target: id(2)}.Append(nil))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ParseConnect(b)
		_, _ = ParseIncoming(b)
		_, _ = ParseAccept(b)
		_, _ = ParseRefuse(b)
		_, _ = ParseConnected(b)
		_, _ = ParseRefused(b)
		_, _ = ParseRegister(b)
		_, _ = ParseRegistered(b)
		_, _ = ParsePeerAddr(b)
	})
}
