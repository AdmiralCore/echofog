package contactswire

import (
	"testing"

	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/services/contacts"
	apitypes "github.com/echofog/echofog-core/pkg/api/types"
)

// The v1 assembly has two ways to hand the network to contacts: the swarm itself, which satisfies
// the port as is (node.go passes n.Swarm), and this nil-safe wrapper. Both must stay valid, or the
// contacts package would have to import the swarm again.
var (
	_ contacts.Wire = (*swarm.Swarm)(nil)
	_ contacts.Wire = New(nil)
)

func TestNilAdapterKnowsNoPeers(t *testing.T) {
	if New(nil).Connected(apitypes.PeerID{1}) {
		t.Fatal("an adapter without a swarm has no reachable peers")
	}
	var absent *Wire
	if absent.Connected(apitypes.PeerID{1}) {
		t.Fatal("a nil adapter has no reachable peers")
	}
}

func TestAdapterAsksTheSwarm(t *testing.T) {
	// a swarm with no connections: nobody is reachable, and asking must not panic on an empty one
	if New(&swarm.Swarm{}).Connected(apitypes.PeerID{1}) {
		t.Fatal("an empty swarm has no reachable peers")
	}
}
