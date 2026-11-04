package contacts

import apitypes "github.com/echofog/echofog-core/pkg/api/types"

// Wire is what the contact list needs from the network: whether a peer is reachable right now.
// Everything else contacts keeps — the record, the alias, the block and the message request — is
// local, so the port is deliberately narrow.
//
// It is declared here, by the consumer, so this package never imports a transport (T7.0, AD1/AD2).
// In v1 the swarm itself satisfies the port as is (Connected); the named adapter over it lives in
// internal/services/contactswire, and v2 brings its own adapter (T7.5b).
type Wire interface {
	// Connected reports whether there is a live session with the peer.
	Connected(id apitypes.PeerID) bool
}
