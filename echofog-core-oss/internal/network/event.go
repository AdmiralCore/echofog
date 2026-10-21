package network

import (
	"net/netip"

	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type NewConnEvent struct {
	Conn       MuxConn
	IsOut      bool
	PeerID     types.PeerID
	PeerPubKey types.PeerPublicKey
	Addr       string
	// Dialable: Addr is where the peer accepts connections (false for inbound TCP, whose
	// source port is ephemeral)
	Dialable bool
	// Caps is what the peer announced it offers (relay, signaling, DHT)
	Caps handshake.Capability
	// RemoteIP is the address the connection came from (for WebSocket behind a CDN, the client's
	// address the CDN reports); per-address peer limits and bans count against it
	RemoteIP netip.Addr
}
