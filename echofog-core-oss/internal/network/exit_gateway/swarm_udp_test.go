package exit_gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Over real QUIC the session switches to datagrams; over the TCP fallback (see
// TestExitAndMediaOverTCPFallback) it stays on the stream.
func TestUDPDatagramsThroughRealSwarm(t *testing.T) {
	exitSwarm, exitTr := newSwarm(t)
	clientSwarm, clientTr := newSwarm(t)
	AttachToSwarm(exitSwarm, openExit())
	require.NoError(t, clientTr.DialEarly(context.Background(), exitTr.Addr().String()))
	require.Eventually(t, func() bool {
		return len(exitSwarm.GetAllPeers()) > 0 && len(clientSwarm.GetAllPeers()) > 0
	}, 5*time.Second, 50*time.Millisecond)
	d := NewP2PDialer(SwarmOpener(clientSwarm, clientSwarm.GetAllPeers()[0].ID()))

	echo := bigUDPEcho(t).LocalAddr().String()
	u, err := d.DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u.Close()
	require.True(t, u.Datagrams())
	for _, size := range []int{100, 1400, 3000, 20000} {
		msg := make([]byte, size)
		_, _ = rand.Read(msg)
		require.NoError(t, u.WriteTo(msg, echo))
		got, _ := readWithin(t, u, 3*time.Second)
		require.True(t, bytes.Equal(msg, got), "size %d", size)
	}

	// two sessions at once keep their datagrams apart
	u2, err := d.DialExitUDP(context.Background())
	require.NoError(t, err)
	defer u2.Close()
	require.NoError(t, u2.WriteTo([]byte("second"), echo))
	got, _ := readWithin(t, u2, 3*time.Second)
	assert.Equal(t, "second", string(got))
	require.NoError(t, u.WriteTo([]byte("first"), echo))
	got, _ = readWithin(t, u, 3*time.Second)
	assert.Equal(t, "first", string(got))
}
