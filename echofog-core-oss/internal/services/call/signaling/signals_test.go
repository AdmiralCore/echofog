package signaling

import (
	"testing"

	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignaling_EncodingDecoding(t *testing.T) {
	var sender types.PeerID
	sender[0] = 0xAA

	// Test Announce
	announce := StreamAnnouncePayload{
		StreamID:    "screen-123",
		Broadcaster: sender,
		MediaType:   "screen",
		BitrateKbps: 3500,
	}

	bytes, err := EncodeSignal(ActionStreamAnnounce, "room-1", sender, 1, announce)
	require.NoError(t, err)

	env, err := DecodeSignal(bytes)
	require.NoError(t, err)
	assert.Equal(t, ActionStreamAnnounce, env.Action)
	assert.Equal(t, "room-1", env.CallID)
	assert.Equal(t, sender, env.Sender)
	assert.Equal(t, uint64(1), env.Epoch)

	// Test VAD
	vad := VADPayload{
		Speaker:  sender,
		Speaking: true,
		AudioDb:  -18.5,
	}
	vBytes, err := EncodeSignal(ActionVADState, "room-1", sender, 1, vad)
	require.NoError(t, err)

	vEnv, err := DecodeSignal(vBytes)
	require.NoError(t, err)
	assert.Equal(t, ActionVADState, vEnv.Action)

	// Test Subscribe
	sub := StreamSubscribePayload{
		StreamID:   "screen-123",
		Subscriber: sender,
		Profile:    tree.PeerNodeProfile{PeerID: sender, UploadKbps: 5000},
	}
	sBytes, err := EncodeSignal(ActionStreamSubscribe, "room-1", sender, 1, sub)
	require.NoError(t, err)
	sEnv, err := DecodeSignal(sBytes)
	require.NoError(t, err)
	assert.Equal(t, ActionStreamSubscribe, sEnv.Action)

	// Decode empty or invalid data
	_, err = DecodeSignal(nil)
	assert.Error(t, err)
	_, err = DecodeSignal([]byte("invalid json"))
	assert.Error(t, err)
}
