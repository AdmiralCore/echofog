package media

import (
	"bytes"
	"testing"
)

// FuzzDecodePacket: every media fragment of a call arrives as a datagram from a peer or a relay.
// Decoding must not panic, and what decodes must encode back to the same bytes.
func FuzzDecodePacket(f *testing.F) {
	p := &MediaPacket{Kind: 1, Seq: 7, Timestamp: 9, StreamID: "s1", FragIdx: 0, FragCount: 2, Payload: []byte("data")}
	f.Add(p.Encode())
	f.Add([]byte{})
	f.Add(make([]byte, fixedHeaderLen+fragFieldsLen))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Decode(data)
		if err != nil {
			return
		}
		if out := p.Encode(); !bytes.Equal(out, data) {
			t.Fatalf("decode/encode changed the fragment:\n in %x\nout %x", data, out)
		}
	})
}
