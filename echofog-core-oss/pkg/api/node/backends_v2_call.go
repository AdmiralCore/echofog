package node

import (
	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// v2Calls is the part of *app.Calls the protocol 2 calls backend uses (T9.4).
type v2Calls interface {
	Manager() *call.CallManager
	Self() types.PeerID
	Join(callID, coordinator string, uploadKbps int, backdrop string) (string, error)
	Leave() error
	NetDrops() (out, in uint64)
}

var _ v2Calls = (*app.Calls)(nil)

// v2CallBackend is the protocol 2 calls and streams backend. Group and room calls, the call path
// and the relay choice stay pending: they need the v2 chat and relay layers.
type v2CallBackend struct {
	v2PendingBackend
	calls v2Calls
}

// AttachV2Calls gives the facade the v2 call layer: calls, streams and the capture of the screen
// answer from it from now on. At protocol 1 it does nothing.
func (n *NodeApi) AttachV2Calls(c v2Calls) {}
