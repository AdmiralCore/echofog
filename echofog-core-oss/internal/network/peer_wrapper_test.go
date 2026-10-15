package network

import (
	"testing"

	net_types "github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// A panic raised inside a data handler must be recovered and the frame dropped, so a malicious or
// malformed frame cannot crash the node. A later well-behaved frame must still be delivered.
func TestDispatchDataRecoversFromHandlerPanic(t *testing.T) {
	var calls int
	p := &PeerWrapper{
		peerID: types.PeerID{},
		onData: func(net_types.MessageType, []byte, types.PeerID) {
			calls++
			panic("handler blew up")
		},
	}

	// Must not propagate the panic.
	p.dispatchData(net_types.TypeGossip, []byte("payload"))

	if calls != 1 {
		t.Fatalf("expected handler to be invoked once, got %d", calls)
	}

	// The wrapper must still be usable for subsequent frames.
	p.dispatchData(net_types.TypeGossip, []byte("another"))
	if calls != 2 {
		t.Fatalf("expected handler to be invoked twice, got %d", calls)
	}
}
