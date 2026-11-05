package node

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// NodeID is an account identity (README §5.2): SHA-256 of the account akey. Contacts and chats
// are keyed by it.
type NodeID [32]byte

// DeviceID is a device identity: SHA-256 of the device akey. The network addresses devices, so
// sessions and cards are keyed by it (E3 introduction).
type DeviceID [32]byte

func (n NodeID) String() string   { return fmt.Sprintf("%x", n[:]) }
func (d DeviceID) String() string { return fmt.Sprintf("%x", d[:]) }

// Peer is who is on the other end of a stream: the authenticated node and device, the verified
// card, the session id and the path that carries the stream (02-architecture §3.1).
type Peer struct {
	// Node is the peer's account identity; contacts and chats are keyed by it.
	Node NodeID
	// Device is the peer's device identity; the network and the session table use it.
	Device DeviceID
	// Card is the verified card of the peer. It is never nil on an established session.
	Card *objects.Card
	// SID is the session id (handshake.md §9); mailbox FETCH and ACK are bound to it.
	SID [16]byte
	// Path says whether the stream travels directly or through a relay (wire.md §8).
	Path mux.PathKind
	// Remote is the carrier address of the active path when the carrier exposes one.
	Remote netip.AddrPort
	// Local is the local identity the session was established under: the main one, a throwaway
	// one, or an identity for one exit (services.md §7).
	Local session.LocalIdentity
}

// Handler serves one stream of a service. It owns the stream and must end it. It is the
// service.Handler of the L3 layer plus the authenticated peer of the session.
type Handler func(ctx context.Context, p Peer, st *mux.Stream)
