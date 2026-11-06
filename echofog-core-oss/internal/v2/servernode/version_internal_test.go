package servernode

import (
	"fmt"
	"testing"
)

// FuzzParseVersion: a parsed version prints back to something that parses to the same numbers, and
// the order of two versions is antisymmetric.
func FuzzParseVersion(f *testing.F) {
	for _, s := range []string{"0.3.0", "v1.2.3", "dev", "1.2", "01.2.3", "1.2.3-rc1", "999999.0.0", "", "1.2.3.4", "-1.0.0"} {
		f.Add(s, "0.3.0")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		va, ok := parseVersion(a)
		if !ok {
			return
		}
		again, ok := parseVersion(fmt.Sprintf("%d.%d.%d", va[0], va[1], va[2]))
		if !ok || again != va {
			t.Fatalf("%q: %v does not print back", a, va)
		}
		if vb, ok := parseVersion(b); ok && cmpVersion(va, vb) != -cmpVersion(vb, va) {
			t.Fatalf("order of %q and %q is not antisymmetric", a, b)
		}
	})
}
