package node_test

import (
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
)

// TestNarrowPathMarkTellsTheUserAndFollowsTheNetwork: a narrow-path mark raises an event in Russian
// (neutral form) that says what the connection does about it, belongs to its network, and is
// forgotten when the network changes, as the freeze mark is (T3.12, T6.4d).
func TestNarrowPathMarkTellsTheUserAndFollowsTheNetwork(t *testing.T) {
	events := &captureEvents{}
	a := newTestNodeWith(t, cards.Hidden, false, nil, events, nil)
	a.rt.SetNetworkID("net-a")
	np := node.NarrowPathOf(a.rt)
	np.Mark("net-a", carrier.ReasonMSS)
	e, ok := events.find(node.EventNarrowPath)
	if !ok {
		t.Fatal("no narrow-path event for the UI")
	}
	for _, want := range []string{"Путь сети узкий", "MSS", "MTU 1200"} {
		if !strings.Contains(e.Message, want) {
			t.Fatalf("message %q lacks %q", e.Message, want)
		}
	}
	if !np.Marked("net-a") {
		t.Fatal("not marked")
	}
	a.rt.SetNetworkID("net-b")
	if np.Marked("net-a") {
		t.Fatal("the mark survived a change of network")
	}
}
