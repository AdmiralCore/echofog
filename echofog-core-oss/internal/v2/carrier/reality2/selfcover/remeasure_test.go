package selfcover

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/carrier/reality2/profile"
)

// TestCompareOwnIsStrictAboutUnstableOurSide: an item that is unstable on our side only is a
// difference, packing included. The own-cover comparison allows only what the website itself
// varies and the length slack; instability on our side is never forgiven (T2.8 F1).
func TestCompareOwnIsStrictAboutUnstableOurSide(t *testing.T) {
	for _, item := range []string{profile.ItemPacking, profile.ItemCipher, profile.ItemGroup, profile.ItemExtOrder, profile.ItemMsgs, profile.ItemTicketCount} {
		diffs, _ := CompareOwn(&profile.CoverProfile{}, &profile.CoverProfile{Unstable: []string{item}})
		if len(diffs) != 1 || diffs[0].Item != item {
			t.Errorf("%s unstable on our side only: diffs %v, want one", item, diffs)
		}
	}
}
