//go:build productgate

package behaviour

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// These tests say what the product must do. They fail today, and that is the point: they are the
// explicit red gate "waits for a fix of the product" (the findings of T15.0c, the product is TEO's).
// `go test -tags productgate ./testbed/behaviour/` runs them; the ordinary run does not, so the
// lab's own suite stays green while the product is open.

// The rows of the policy table for a foreign destination that set Rotate need a byte limit the
// freeze cannot reach, or a 1 GiB transfer stops at the freeze when UDP is gone.
func TestProductAGigabyteSurvivesAFreezeWhenUDPIsGone(t *testing.T) {
	both := CensorConfig{FreezeBytes: 16384, UDPDrop: true}
	for _, p := range []Product{ruWired, cnWired} {
		r := MeasureFreeze(p, both, 1<<30)
		assert.True(t, r.Completed, "%s: stops at %d bytes (%s)", r.Rule, r.FrozenAt, r.Via)
	}
}

// noise3 has to open on a path of 1280 bytes and of 1200: the first flight must fit the floor the
// specification names (noise3/doc.go).
func TestProductNoise3OpensOnASmallPath(t *testing.T) {
	for _, mtu := range []int{1280, 1200} {
		r, _ := transfer(t, NetemConfig{Seed: 6, MTU: mtu - 28}, 32<<10, nil)
		assert.True(t, r.Connected, "MTU %d: %s", mtu, r.Err)
	}
}
