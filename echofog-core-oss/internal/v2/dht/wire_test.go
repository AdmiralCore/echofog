package dht

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// TestFrameVectorFindNode pins the dht/2 FIND_NODE vector of frames.json: req=7, target 00..1f.
func TestFrameVectorFindNode(t *testing.T) {
	raw, err := os.ReadFile("../testdata/frames.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var doc struct {
		Frames []struct {
			Name    string `json:"name"`
			Type    uint64 `json:"type"`
			Flags   byte   `json:"flags"`
			Payload string `json:"payload"`
			Encoded string `json:"encoded"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range doc.Frames {
		if v.Name != "dht/2 FIND_NODE" {
			continue
		}
		found = true
		payload, _ := hex.DecodeString(v.Payload)
		want, _ := hex.DecodeString(v.Encoded)

		var target [32]byte
		for i := range target {
			target[i] = byte(i)
		}
		got, err := MarshalFindNode(7, target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("payload %x, want %x", got, payload)
		}
		enc := frame.AppendFrame(nil, frame.Frame{Type: v.Type, Flags: v.Flags, Payload: got})
		if !bytes.Equal(enc, want) {
			t.Fatalf("encoded %x, want %x", enc, want)
		}
		spec, _ := service.Builtin(service.DHT)
		rd := frame.NewReader(bytes.NewReader(want), spec.Versions[Version].Limiter())
		f, err := rd.ReadFrame()
		if err != nil {
			t.Fatalf("read with the dht/2 limiter: %v", err)
		}
		m, err := unmarshalFindNode(f.Payload)
		if err != nil || m.Req != 7 || !bytes.Equal(m.Target, target[:]) {
			t.Fatalf("decode: %+v %v", m, err)
		}
	}
	if !found {
		t.Fatal("vector not found")
	}
}

// TestDecodersRejectBadInput: non-canonical or wrong-length bodies never decode.
func TestDecodersRejectBadInput(t *testing.T) {
	if _, err := unmarshalFindNode([]byte{0x08, 0x01, 0x12, 0x01, 0x00}); err == nil {
		t.Fatal("short target accepted")
	}
	if _, err := unmarshalFindValue(nil); err == nil {
		t.Fatal("empty FIND_VALUE accepted")
	}
	// Non-canonical: field 2 before field 1.
	if _, err := unmarshalStore([]byte{0x12, 0x00, 0x08, 0x01}); err == nil {
		t.Fatal("out-of-order STORE accepted")
	}
}

// FuzzFrames covers every dht/2 decoder: no panic, no unbounded allocation.
func FuzzFrames(f *testing.F) {
	f.Add(byte(0), []byte{})
	f.Add(byte(0), []byte{0x08, 0x07, 0x12, 0x20})
	f.Add(byte(2), []byte{0x08, 0x01, 0x12, 0x00})
	f.Add(byte(4), []byte{0x08, 0x01, 0x10, 0x04})
	f.Add(byte(6), []byte{0x08, 0x01, 0x12, 0x02, 0x01, 0x02})
	f.Fuzz(func(t *testing.T, kind byte, b []byte) {
		switch kind % 8 {
		case 0:
			_, _ = unmarshalFindNode(b)
		case 1:
			_, _ = UnmarshalNodes(b)
		case 2:
			_, _ = unmarshalFindValue(b)
		case 3:
			_, _ = UnmarshalValue(b)
		case 4:
			_, _ = unmarshalStore(b)
		case 5:
			_, _ = UnmarshalStored(b)
		case 6:
			_, _ = UnmarshalError(b)
		default:
			_, _ = UnmarshalNodes(b)
		}
	})
}
