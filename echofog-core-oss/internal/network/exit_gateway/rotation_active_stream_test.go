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

// Rotation must not force-close a transfer that is still in flight. The swarm keeps one connection
// per peer, so dialing a fresh connection to rotate the 5-tuple replaces and (after a drain
// timeout) force-closes the connection the transfer rides. A single large download is one stream
// that cannot be split across connections, so it would die. This test, over two real swarms,
// drives a long stream past the pool's byte budget, opens a small stream next to it (what triggers
// rotation), and asserts the 5-tuple did not rotate and the long stream still carries data. Once
// nothing is in flight, rotation is free to dial a fresh 5-tuple again.
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

	// The 5-tuple to the exit must not have rotated while the big stream is in flight.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, link0, clientSwarm.LinkOf(exitID),
		"connection must not rotate (be force-closed) while a stream is still transferring")

	// And the big stream is still alive: it keeps carrying data after the rotation trigger.
	roundTrip(big, 256)

	require.NoError(t, small.Close())
	require.NoError(t, big.Close())

	// With nothing in flight, push the reusable session past its budget, then one more stream is
	// free to rotate to a fresh 5-tuple.
	time.Sleep(20 * time.Millisecond)
	filler, err := dialer.DialExitStream(ctx, target)
	require.NoError(t, err)
	roundTrip(filler, 1500)
	require.NoError(t, filler.Close())
	time.Sleep(50 * time.Millisecond)

	trigger, err := dialer.DialExitStream(ctx, target)
	require.NoError(t, err)
	defer trigger.Close()
	require.Eventually(t, func() bool {
		return clientSwarm.LinkOf(exitID) != link0
	}, 5*time.Second, 100*time.Millisecond,
		"rotation should dial a fresh 5-tuple once no stream is in flight")
}
