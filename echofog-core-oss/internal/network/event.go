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
	// Reality is true when this is an outbound QUIC-REALITY connection (dialed with a camouflage
	// SNI, so the wire looks like TLS to a cover site). A noise/plain connection is false. The exit
	// pool uses it to keep every link to a camouflage exit on REALITY and never carry tunnel
	// traffic over a noise connection a censor would flag.
	Reality bool
	// RemoteIP is the address the connection came from (for WebSocket behind a CDN, the client's
	// address the CDN reports); per-address peer limits and bans count against it
	RemoteIP netip.Addr
}
