// Package contactswire is the v1 assembly of the contacts port on top of the swarm.
//
// The contacts package holds the records and declares Wire, the one question it asks the network
// (whether a peer is reachable). This package is the only place that names the p2p layer for it,
// so the service itself stays free of it (T7.0, AD1/AD2). The swarm already satisfies the port as
// is; the wrapper adds the nil checks for the places that build a service without a network, and
// gives v2's adapter (T7.5b) a named counterpart to sit next to.
package contactswire

import (
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	apitypes "github.com/echofog/echofog-core/pkg/api/types"
)

// Wire is the swarm seen through the contacts port.
type Wire struct {
	sw *swarm.Swarm
}

// New wraps the v1 swarm. A nil swarm is allowed (a server-role node, tests): such a wire knows no
// reachable peer.
func New(sw *swarm.Swarm) *Wire {
	return &Wire{sw: sw}
}

// Connected reports whether the swarm holds a live link to the peer.
func (w *Wire) Connected(id apitypes.PeerID) bool {
	return w != nil && w.sw != nil && w.sw.Connected(id)
}
