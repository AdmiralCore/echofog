package selfcover

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/carrier/reality2/profile"
)

// TestCompareOwnPackingInstabilityIsSymmetric: how a flight is cut into TCP segments or QUIC
// packets depends on timing, and the selected connection runs through the gate, so under load it
// varies in a window where the website's longer measurement happened not to. The own cover is one
// stack, so instability of the packing on either side is not a difference (T2.6b-2 F2). Any other
// item that is unstable on our side only still is one.
func TestCompareOwnPackingInstabilityIsSymmetric(t *testing.T) {
	site := &profile.CoverProfile{}
	ours := &profile.CoverProfile{Unstable: []string{profile.ItemPacking}}
	diffs, unstable := CompareOwn(site, ours)
	if len(diffs) != 0 {
		t.Fatalf("packing unstable on our side only is a difference: %v", diffs)
	}
	if len(unstable) != 1 || unstable[0] != profile.ItemPacking {
		t.Fatalf("unstable = %v, want [packing]", unstable)
	}
	for _, item := range []string{profile.ItemCipher, profile.ItemGroup, profile.ItemExtOrder, profile.ItemMsgs, profile.ItemTicketCount} {
		diffs, _ := CompareOwn(&profile.CoverProfile{}, &profile.CoverProfile{Unstable: []string{item}})
		if len(diffs) != 1 || diffs[0].Item != item {
			t.Errorf("%s unstable on our side only: diffs %v, want one", item, diffs)
		}
	}
}
