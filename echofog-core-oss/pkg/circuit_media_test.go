package pkg

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The audio and video of a call between two peers behind a relay cross it as datagrams; the
// signaling of the same circuit crosses it as streams. Both arrive.
func TestCircuitMediaTravelsAsDatagrams(t *testing.T) {
	aN, a := startNodeWith(t, "cm-a", nil)
	bN, b := startNodeWith(t, "cm-b", nil)
	rN, r := startNodeWith(t, "cm-relay", func(c *config.AppConfig) { c.Relay.Server.Enabled = true })

	join := network.JoinPeerAddr(rN.Transport.Addr().String(), rN.ID)
	require.NoError(t, a.ConnectToPeer(join))
	require.NoError(t, b.ConnectToPeer(join))
	require.Eventually(t, func() bool { return len(r.GetPeers()) >= 2 }, 5*time.Second, 50*time.Millisecond)

	var audio, signaling atomic.Int64
	bN.RelayClient.AddDataListener(func(_ uint32, ty net_types.MessageType, _ []byte, _ types.PeerID) {
		switch ty {
		case net_types.TypeCallMediaAudio:
			audio.Add(1)
		case net_types.TypeCallSignaling:
			signaling.Add(1)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cid, err := aN.RelayClient.OpenCircuit(ctx, rN.ID, bN.ID)
	require.NoError(t, err)

	const media, messages = 100, 20
	for i := 0; i < media; i++ {
		require.NoError(t, aN.RelayClient.SendCircuitData(cid, net_types.TypeCallMediaAudio, make([]byte, 400)))
		time.Sleep(2 * time.Millisecond)
	}
	for i := 0; i < messages; i++ {
		require.NoError(t, aN.RelayClient.SendCircuitData(cid, net_types.TypeCallSignaling, make([]byte, 40)))
	}

	// datagrams may be lost on a busy machine, messages may not
	require.Eventually(t, func() bool { return signaling.Load() == messages }, 5*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { return audio.Load() >= media*9/10 }, 5*time.Second, 20*time.Millisecond)

	byDatagram := rN.RelayServer.Stats().ByDatagram
	assert.GreaterOrEqual(t, byDatagram, uint64(media*9/10), "the relay passed the media on as datagrams")
	assert.LessOrEqual(t, byDatagram, uint64(media), "the signaling did not go that way")
}
