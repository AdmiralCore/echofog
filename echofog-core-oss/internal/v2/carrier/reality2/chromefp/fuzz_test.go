package chromefp

import "testing"

func FuzzParse(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = Parse(b) })
}
