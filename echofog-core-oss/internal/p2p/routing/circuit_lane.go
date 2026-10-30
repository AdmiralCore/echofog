package routing

import (
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/net_types"
)

// Frames of a relay circuit travelled one stream each. That is right for messages, which must
// arrive, and wrong for the audio and video of a call: over TCP a stream costs several frames of
// its own, and when the link stalls for a moment thousands of them wait at once, up to the cap
// the link puts on open streams, past which every new one is thrown away. A media frame is meant
// to be lost now and then (the receiver asks again, or for a keyframe), so between peers that
// can take it, it goes as a datagram, as on a direct link.

// circuitFrameHeader is what a circuit puts before the inner frame: the circuit id and the inner type.
const circuitFrameHeader = 5

// mediaByDatagram reports whether a circuit frame of the inner type goes to a peer with caps as a datagram.
func mediaByDatagram(inner net_types.MessageType, caps handshake.Capability) bool {
	return net_types.IsUnreliableType(inner) && caps.Has(handshake.CapCircuitMedia)
}

// innerType is the type of the inner frame of a circuit data payload ([circuit id][type][data]).
func innerType(payload []byte) (net_types.MessageType, bool) {
	if len(payload) < circuitFrameHeader {
		return 0, false
	}
	return net_types.MessageType(payload[4]), true
}
