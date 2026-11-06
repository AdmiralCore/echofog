package route

import "testing"

func FuzzParse(f *testing.F) {
	for _, s := range []string{"", "auto", "direct", "relay", "RELAY ", "tor", "\x00relay"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := Parse(s)
		if err != nil {
			return
		}
		// What parses prints back to something that parses to the same route.
		if again, err := Parse(r.String()); err != nil || again != r {
			t.Fatalf("Parse(%q) = %v, but %q does not round-trip: %v %v", s, r, r.String(), again, err)
		}
	})
}
