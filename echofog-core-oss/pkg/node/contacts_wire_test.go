package node

import (
	"testing"

	"github.com/echofog/echofog-core/internal/p2p/swarm"
	apitypes "github.com/echofog/echofog-core/pkg/api/types"
)

// The assembly point of the contacts port in v1 (T7.0). node.go hands the swarm to the service
// directly; the wrapper exists for the places that build a service without a network (a
// server-role node, tests), where a typed nil swarm must not look like a live one.
func TestContactsWire(t *testing.T) {
	if contactsWire(nil) != nil {
		t.Fatal("a node without a swarm has no contacts wire")
	}
	w := contactsWire(&swarm.Swarm{})
	if w == nil {
		t.Fatal("a node with a swarm must get a wire")
	}
	if w.Connected(apitypes.PeerID{1}) {
		t.Fatal("an empty swarm has no reachable peers")
	}
}
