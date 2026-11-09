package bootstrap_test

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
)

// TestSourceTreeEmbedsNoOfficialList reads the actual file embedded by builtin.go. The OSS source
// tree has no addresses: filling release/list.bin with a T5.5 line must make this test red, so the
// empty built-in list of a source build is an invariant and not a tautology.
func TestSourceTreeEmbedsNoOfficialList(t *testing.T) {
	if raw := bootstrap.BuiltinRaw(); len(raw) != 0 {
		t.Fatalf("the OSS source tree embeds %d bytes in release/list.bin: the official list belongs to a release build", len(raw))
	}
}
