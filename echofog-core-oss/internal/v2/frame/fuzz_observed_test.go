package frame

import (
	"bytes"
	"testing"
)

// FuzzParseObservedAddr: whatever decodes re-encodes to the same bytes, and nothing panics.
func FuzzParseObservedAddr(f *testing.F) {
	for _, s := range []string{"04080808081f90", "062606470000000000000000000011110001", "", "04", "0501020304ffff", "0401020304"} {
		f.Add(unhex(f, s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		ap, err := ParseObservedAddr(b)
		if err != nil {
			return
		}
		if enc := AppendObservedAddr(nil, ap); !bytes.Equal(enc, b) {
			t.Fatalf("%x -> %v -> %x", b, ap, enc)
		}
	})
}
