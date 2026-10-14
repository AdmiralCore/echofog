package tonwallet

import (
	"testing"
)

// FuzzParseUnits: an amount typed by a person (or pasted from anywhere) becomes money that is
// sent. It must never be zero, negative or different from what formatting it back says.
func FuzzParseUnits(f *testing.F) {
	for _, s := range []string{"1", "0.5", "0,5", ".5", "5.", "1.000000001", "0", "-1", "+1", "1e3", "0x10", "", " 2 ", "1.2.3", "٣", "1_000", "00012.50"} {
		f.Add(s, 6)
		f.Add(s, 9)
	}
	f.Fuzz(func(t *testing.T, s string, decimals int) {
		if decimals < 0 || decimals > 18 {
			t.Skip()
		}
		n, err := ParseUnits(s, decimals)
		if err != nil {
			return
		}
		if n.Sign() <= 0 {
			t.Fatalf("%q parsed to %v", s, n)
		}
		back, err := ParseUnits(FormatUnits(n, decimals), decimals)
		if err != nil || back.Cmp(n) != 0 {
			t.Fatalf("%q -> %v -> %q -> %v (%v)", s, n, FormatUnits(n, decimals), back, err)
		}
	})
}
