package relay

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

func id(b byte) [32]byte { var x [32]byte; x[0] = b; return x }

// Only connected relay-flag nodes are candidates; relays B is known to use come first (they are
// the ones A and B can both reach); excluded ones are dropped.
func TestChooseOrdersCommonRelaysFirst(t *testing.T) {
	cands := []Candidate{
		{Node: id(1), Device: id(1), Flags: objects.FlagRelay, Connected: true},  // PEX relay, B unknown
		{Node: id(2), Device: id(2), Flags: objects.FlagRelay, Connected: true},  // relay of B
		{Node: id(3), Device: id(3), Flags: 0, Connected: true},                  // not a relay
		{Node: id(4), Device: id(4), Flags: objects.FlagRelay, Connected: false}, // no session with it
		{Node: id(5), Device: id(5), Flags: objects.FlagRelay, Connected: true},  // excluded
		{Node: id(6), Device: id(6), Flags: objects.FlagRelay, Connected: true},  // relay of B
	}
	common := map[[32]byte]struct{}{id(2): {}, id(6): {}, id(5): {}}
	got := Choose(cands, common, map[[32]byte]struct{}{id(5): {}}, nil)
	want := [][32]byte{id(2), id(6), id(1)}
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
		{Node: id(1), Device: id(1), Flags: objects.FlagRelay, Connected: true},
		{Node: id(2), Device: id(2), Flags: objects.FlagRelay, Connected: true},
		{Node: id(3), Device: id(3), Flags: objects.FlagRelay, Connected: true},
	}
	last := func(n int) int { return n - 1 } // always picks the last: deterministic, not the identity
	a := Choose(cands, nil, nil, last)
	b := Choose(cands, nil, nil, nil)
	if len(a) != 3 || len(b) != 3 {
		t.Fatal("lost candidates")
	}
	if a[0].Node == b[0].Node && a[1].Node == b[1].Node && a[2].Node == b[2].Node {
		t.Fatal("the injected random source had no effect on the order")
	}
}
