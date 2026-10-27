package exit_gateway

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasActiveStreams(t *testing.T) {
	cfg := config.DefaultFlowShapingConfig()
	cfg.MinDialInterval = time.Millisecond
	pool := NewPeerSessionPoolWithDialer(types.PeerID{}, cfg,
		func(ctx context.Context) (StreamOpener, func() error, error) {
			return func(ctx context.Context) (FramedStream, error) { return newMockStream(), nil }, nil, nil
		})
	defer pool.Close()

	assert.False(t, pool.HasActiveStreams(), "no streams opened yet")

	st, err := pool.OpenStream(context.Background())
	require.NoError(t, err)
	assert.True(t, pool.HasActiveStreams(), "a stream is open")

	require.NoError(t, st.Close())
	assert.False(t, pool.HasActiveStreams(), "the only stream is closed")
}

// Rotation must not force-close a transfer that is still in flight. An exit now runs a parallel
// session pool: a rotation dials a fresh connection (a new 5-tuple), announced as a pool link, and
// both ends add it alongside the others, draining only idle links, so the connection a transfer
// rides is never cut off. A single large download is one stream that cannot be split across
// connections, so force-closing its connection would kill it. This test, over two real swarms,
// drives a long stream past the pool's byte budget, opens a small stream next to it (what triggers
// rotation), and asserts that both ends now hold two links, the link the big transfer rides is
// still the primary, and the long stream still carries data.
func TestRotation_DoesNotForceCloseActiveStream(t *testing.T) {
	ResetSwarmPools()
	t.Cleanup(ResetSwarmPools)

	exitSwarm, exitTr := newSwarm(t)
	clientSwarm, clientTr := newSwarm(t)

	svc := NewExitService()
	svc.allowPrivate.Store(true)
	svc.SetEnabled(true)
	AttachToSwarm(exitSwarm, svc)

	require.NoError(t, clientTr.DialEarly(context.Background(), exitTr.Addr().String()))
	require.Eventually(t, func() bool {
		return len(exitSwarm.GetAllPeers()) > 0 && len(clientSwarm.GetAllPeers()) > 0
	}, 5*time.Second, 50*time.Millisecond)
	exitID := clientSwarm.GetAllPeers()[0].ID()

	target := echoServer(t)

	// Tiny byte budget so a little traffic makes a session want to rotate; lifetime/stream caps
	// kept high so only the byte budget drives rotation (deterministic).
	cfg := config.FlowShapingConfig{
		MaxConnections:     3,
		MaxConcurrency:     8,
		MaxBytesMin:        1000,
		MaxBytesMax:        1000,
		MaxConnLifetimeMin: time.Hour,
		MaxConnLifetimeMax: time.Hour,
		MaxStreamsPerConn:  500,
		MinDialInterval:    10 * time.Millisecond,
	}
	dialer := NewP2PDialer(SwarmOpenerWithConfig(clientSwarm, exitID, cfg))

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	roundTrip := func(s io.ReadWriteCloser, n int) {
		t.Helper()
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i)
		}
		_, err := s.Write(payload)
		require.NoError(t, err)
		got := make([]byte, n)
		_, err = io.ReadFull(s, got)
		require.NoError(t, err)
		require.Equal(t, payload, got)
	}

	// A long-lived stream carrying a transfer. Push its session past the byte budget (writes and the
	// echoed reads are both counted) so the next stream opened next to it triggers a rotation.
	big, err := dialer.DialExitStream(ctx, target)
	require.NoError(t, err)
	defer big.Close()
	roundTrip(big, 1500)

	link0 := clientSwarm.LinkOf(exitID)
	require.NotEmpty(t, link0)

	time.Sleep(20 * time.Millisecond) // past MinDialInterval

	// A small stream next to the big one: this is what used to trigger a fresh dial that replaced
	// and force-closed the connection the big transfer rides.
	small, err := dialer.DialExitStream(ctx, target)
	require.NoError(t, err)

	// The rotation adds a fresh parallel link (a new 5-tuple) while the big stream is in flight, and
	// both ends keep it beside the first connection: the exit must not replace the client's first
	// connection with it (that used to drop the client's peer, every stream and the VPN with it).
	require.Eventually(t, func() bool {
		return clientSwarm.GetPeer(exitID).LinkCount() == 2
	}, 5*time.Second, 50*time.Millisecond, "rotation should add a parallel link during the transfer")
	clientID := exitSwarm.GetAllPeers()[0].ID()
	require.Eventually(t, func() bool {
		p := exitSwarm.GetPeer(clientID)
		return p != nil && p.LinkCount() == 2
	}, 5*time.Second, 50*time.Millisecond, "the exit should keep the client's pool link beside its first connection")
	assert.Equal(t, link0, clientSwarm.LinkOf(exitID),
		"the connection the transfer rides must stay the primary, never be replaced")

	// And the big stream is still alive: it keeps carrying data after the rotation.
	roundTrip(big, 256)

	require.NoError(t, small.Close())
	require.NoError(t, big.Close())
}
