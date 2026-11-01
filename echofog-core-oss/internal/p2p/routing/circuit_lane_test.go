package routing

import (
	"testing"

	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/stretchr/testify/assert"
)

func TestMediaByDatagram(t *testing.T) {
	media := []net_types.MessageType{net_types.TypeCallMediaAudio, net_types.TypeCallMediaVideo}
	messages := []net_types.MessageType{net_types.TypeCallSignaling, net_types.TypeChatMessage,
		net_types.TypePing, net_types.TypePong, net_types.TypeCircuitClose}
	withBit := handshake.CapDHTServer | handshake.CapCircuitMedia

	for _, ty := range media {
		assert.True(t, mediaByDatagram(ty, withBit), "media goes as a datagram to a peer that takes it")
		assert.False(t, mediaByDatagram(ty, handshake.CapDHTServer), "an older peer gets a stream")
		assert.False(t, mediaByDatagram(ty, handshake.CapNone), "a node that announced nothing gets a stream")
	}
	for _, ty := range messages {
		assert.False(t, mediaByDatagram(ty, withBit), "a message must arrive: it goes by stream")
	}
}

func TestInnerType(t *testing.T) {
	_, ok := innerType([]byte{0, 0, 0, 1})
	assert.False(t, ok, "a payload shorter than the circuit header has no inner type")

	ty, ok := innerType([]byte{0, 0, 0, 7, byte(net_types.TypeCallMediaVideo), 1, 2, 3})
	assert.True(t, ok)
	assert.Equal(t, net_types.TypeCallMediaVideo, ty)
}

// What a datagram may carry: the unreliable types and the data of a circuit, nothing else.
func TestDatagramFrames(t *testing.T) {
	assert.True(t, net_types.IsDatagramFrame(net_types.TypeCallMediaAudio))
	assert.True(t, net_types.IsDatagramFrame(net_types.TypeCallMediaVideo))
	assert.True(t, net_types.IsDatagramFrame(net_types.TypeCircuitData))
	for _, ty := range []net_types.MessageType{net_types.TypeChatMessage, net_types.TypeCircuitOpenRequest,
		net_types.TypeCircuitClose, net_types.TypeCallSignaling, net_types.TypeStore} {
		assert.False(t, net_types.IsDatagramFrame(ty), "control traffic stays on reliable streams")
	}
}
