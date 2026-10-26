package handshake

import "github.com/echofog/echofog-core/pkg/api/types"

type Capability uint32

const (
	CapNone         Capability = 0
	CapRelay        Capability = 1 << 0
	CapBridge       Capability = 1 << 1
	CapDHTServer    Capability = 1 << 2
	CapPrivateNet   Capability = 1 << 3
	CapHeavyCompute Capability = 1 << 4
	// CapSignal: the node passes on the few small messages that start a direct connection
	// between peers behind NATs (hole punching), but relays no traffic.
	CapSignal Capability = 1 << 5
	// CapTunnelPadding: the node speaks the padded exit-tunnel data phase (Vision-style framing
	// that breaks the TLS-in-TLS size/timing fingerprint). A node announces it only when it is
	// willing to strip that framing (anti_dpi.tunnel_padding on), so a peer may pad to it iff it
	// sees this bit. Both sides compute the same decision: localToggle && peer.Caps().Has(this).
	CapTunnelPadding Capability = 1 << 6
)

// DefaultCaps is what a node announces unless told otherwise: it relays and serves the DHT.
const DefaultCaps = CapRelay | CapDHTServer

type HandshakeResult struct {
	PeerID    types.PeerID
	PubKey    types.PeerPublicKey
	NetworkID [32]byte
	Caps      Capability
	Version   uint32
	// ListenPort is where the initiator accepts connections, as it announced (responder side
	// only; 0 when it announced none).
	ListenPort uint16
}

func (c Capability) Has(flag Capability) bool {
	return c&flag != 0
}
