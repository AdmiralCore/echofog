package relay

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

func nid(b byte) [32]byte { var x [32]byte; x[0] = b; return x }

// Only connected relay-flag nodes are candidates; relays B is known to use come first (they are
// the ones A and B can both reach); excluded ones are dropped.
func TestChooseOrdersCommonRelaysFirst(t *testing.T) {
	cands := []Candidate{
		{Node: nid(1), Device: nid(1), Flags: objects.FlagRelay, Connected: true},  // PEX relay, B unknown
		{Node: nid(2), Device: nid(2), Flags: objects.FlagRelay, Connected: true},  // relay of B
		{Node: nid(3), Device: nid(3), Flags: 0, Connected: true},                  // not a relay
		{Node: nid(4), Device: nid(4), Flags: objects.FlagRelay, Connected: false}, // no session with it
		{Node: nid(5), Device: nid(5), Flags: objects.FlagRelay, Connected: true},  // excluded
		{Node: nid(6), Device: nid(6), Flags: objects.FlagRelay, Connected: true},  // relay of B
	}
	common := map[[32]byte]struct{}{nid(2): {}, nid(6): {}, nid(5): {}}
	got := Choose(cands, common, map[[32]byte]struct{}{nid(5): {}}, nil)
	want := [][32]byte{nid(2), nid(6), nid(1)}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Node != want[i] {
			t.Fatalf("position %d: got %x, want %x", i, got[i].Node[0], want[i][0])
		}
	}
}

func TestChooseShufflesInsideATier(t *testing.T) {
	cands := []Candidate{
		{Node: nid(1), Device: nid(1), Flags: objects.FlagRelay, Connected: true},
		{Node: nid(2), Device: nid(2), Flags: objects.FlagRelay, Connected: true},
		{Node: nid(3), Device: nid(3), Flags: objects.FlagRelay, Connected: true},
	}
	first := func(n int) int { return 0 } // always picks index 0: deterministic, not the identity
	a := Choose(cands, nil, nil, first)
	b := Choose(cands, nil, nil, nil)
	if len(a) != 3 || len(b) != 3 {
		t.Fatal("lost candidates")
	}
	if a[0].Node == b[0].Node && a[1].Node == b[1].Node && a[2].Node == b[2].Node {
		t.Fatal("the injected random source had no effect on the order")
	}
}
