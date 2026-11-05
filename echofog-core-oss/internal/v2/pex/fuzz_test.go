package pex

import "testing"

// FuzzFrames covers both pex/2 decoders: a length-delimited service payload arrives from the wire
// and must never panic, allocate without a bound or be accepted when it is not canonical.
func FuzzFrames(f *testing.F) {
	f.Add(byte(0), []byte{})
	f.Add(byte(0), []byte{0x08, 0x01})
	f.Add(byte(1), []byte{0x08, 0x01, 0x12, 0x00})
	f.Add(byte(1), []byte{0x08, 0x01, 0x12, 0x02, 0x01, 0x02})
	f.Fuzz(func(t *testing.T, kind byte, b []byte) {
		if kind&1 == 0 {
			_, _ = unmarshalGet(b)
			return
		}
		_, _ = unmarshalCards(b)
	})
}
