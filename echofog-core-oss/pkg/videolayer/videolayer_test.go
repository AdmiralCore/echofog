package videolayer

import (
	"testing"

	"github.com/pion/rtp"
)

func marked(t *testing.T, m Mark, others map[uint8][]byte) []byte {
	t.Helper()
	p := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 112, SequenceNumber: 7, Timestamp: 9000, SSRC: 1}, Payload: []byte{0x65, 1, 2, 3}}
	p.Header.Extension, p.Header.ExtensionProfile = true, profileOneByte
	for id, v := range others {
		if err := p.Header.SetExtension(id, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Header.SetExtension(ExtensionID, m.Data()); err != nil {
		t.Fatal(err)
	}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMarkRoundTrip(t *testing.T) {
	want := Mark{TID: 2, Start: true, Discardable: true, Index: 0, Count: 3, L0: 65535, L1: 1}
	b := marked(t, want, nil)
	if n := len(b) - 12 - 4; n != ExtensionLen {
		t.Fatalf("the mark adds %d bytes, ExtensionLen says %d", n, ExtensionLen)
	}
	got, off, ok := Find(b)
	if !ok || got != want {
		t.Fatalf("Find = %+v %v, want %+v", got, ok, want)
	}
	SetCounts(b, off, 10, 20)
	got, _, _ = Find(b)
	if got.L0 != 10 || got.L1 != 20 || got.TID != 2 || got.Count != 3 {
		t.Fatalf("after SetCounts: %+v", got)
	}
	var p rtp.Packet
	if err := p.Unmarshal(b); err != nil || len(p.Payload) != 4 || p.Payload[0] != 0x65 {
		t.Fatalf("the payload moved: %v %x", err, p.Payload)
	}
	if got.Layer(0, 7) != 10 || got.Layer(1, 7) != 20 || got.Layer(All, 7) != 7 {
		t.Fatal("Layer picks the wrong count")
	}
	if (Mark{TID: 1}).Class() != 1 || (Mark{TID: 3}).Class() != All {
		t.Fatal("Class")
	}
}

func TestFindAmongOtherElements(t *testing.T) {
	want := Mark{TID: 1, End: true, Key: false, Index: 4, Count: 5, L0: 3, L1: 4}
	b := marked(t, want, map[uint8][]byte{1: []byte("0"), 3: {1, 2, 3}})
	if got, _, ok := Find(b); !ok || got != want {
		t.Fatalf("Find = %+v %v", got, ok)
	}
}

func TestFindIgnoresUnmarked(t *testing.T) {
	plain, _ := (&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1}, Payload: []byte{1}}).Marshal()
	p := rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1}, Payload: []byte{1}}
	p.Header.Extension, p.Header.ExtensionProfile = true, profileOneByte
	_ = p.Header.SetExtension(ExtensionID, []byte{1, 2}) // same ID, another length: not ours
	other, _ := p.Marshal()
	for name, b := range map[string][]byte{"plain": plain, "foreign": other, "short": {0x90, 0, 0}, "truncated": other[:15]} {
		if _, _, ok := Find(b); ok {
			t.Fatalf("%s: found a mark", name)
		}
	}
}
