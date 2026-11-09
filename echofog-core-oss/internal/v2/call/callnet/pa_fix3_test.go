package callnet

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Under call_route=relay media goes over the relay even when a direct path exists (the signalling
// stream is usually direct), so the peer must get the relay watchdogs: short direct ones would see
// the added hop as a lost parent and rebuild the tree.
func TestRelayedIsTrueWhenRelayRouteUsesADirectSessionToo(t *testing.T) {
	both := &fakeLink{kinds: map[mux.PathKind]int{mux.PathDirect: 1, mux.PathRelay: 1}}
	tr, _ := newT(links{dev(2): both}, route.Relay)
	if !tr.Relayed(dev(2)) {
		t.Fatal("relay route with a direct path reported direct watchdogs")
	}
	// Under Auto media takes the direct path: the peer is not reached only through a relay.
	tr, _ = newT(links{dev(2): both}, route.Auto)
	if tr.Relayed(dev(2)) {
		t.Fatal("auto with a direct path reported a relayed peer")
	}
}
