package swarm

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/natcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A peer reports the address it sees our connection at; on loopback that is our own socket.
func TestPeersReportTheAddressTheySeeUsAt(t *testing.T) {
	ctxA, swA, trA, storeA := testSwarmInit(t, "127.0.0.1:0")
	defer swA.Close()
	defer trA.Close()
	defer storeA.CloseAndClear()
	_, swB, trB, storeB := testSwarmInit(t, "127.0.0.1:0")
	defer swB.Close()
	defer trB.Close()
	defer storeB.CloseAndClear()

	require.NoError(t, trB.DialEarly(ctxA, trA.Addr().String()))
	require.Eventually(t, func() bool { return len(swA.GetAllPeers()) > 0 && len(swB.GetAllPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// B asks A which address it sees B at: B's own QUIC socket
	ap, err := swB.ObservedAddr(ctx, swB.GetAllPeers()[0].ID())
	require.NoError(t, err)
	assert.Equal(t, trB.Addr().String(), ap.String())

	// and A asks B (the connection was opened by B, A sees B's listening address either way)
	ap, err = swA.ObservedAddr(ctx, swA.GetAllPeers()[0].ID())
	require.NoError(t, err)
	assert.Equal(t, trA.Addr().String(), ap.String())

	obs := swB.ObserveFromPeers(ctx, 4)
	require.Len(t, obs, 1)
	assert.Equal(t, trB.Addr().String(), obs[0].Addr.String())
	r := natcheck.Classify(obs, nil)
	assert.Equal(t, natcheck.MappingUnknown, r.Mapping, "a single observer cannot tell the mapping kind")
}

func TestObservedAddrFailsForAnUnknownPeer(t *testing.T) {
	_, swA, trA, storeA := testSwarmInit(t, "127.0.0.1:0")
	defer swA.Close()
	defer trA.Close()
	defer storeA.CloseAndClear()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := swA.ObservedAddr(ctx, [32]byte{9})
	assert.Error(t, err)
	assert.Empty(t, swA.ObserveFromPeers(ctx, 4))
}
