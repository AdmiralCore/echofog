package scenarios

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/behaviour"
)

// The harness is the base of every scenario: real nodes of internal/v2, real noise3 on loopback,
// optionally through the in-process netem.
func TestTwoRealNodesTalkOverNoise3(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w := NewWorld(t.TempDir())
	defer w.Close()
	srv, err := w.Add(NodeSpec{Name: "srv", Server: true, Listen: true})
	require.NoError(t, err)
	cli, err := w.Add(NodeSpec{Name: "cli"})
	require.NoError(t, err)
	require.NoError(t, w.StartAll(ctx))

	peer, sess, err := cli.Dial(ctx, srv)
	require.NoError(t, err)
	assert.Equal(t, srv.ID.DeviceID, peer.Device)
	rtt, err := sess.Ping(ctx)
	require.NoError(t, err)
	assert.Less(t, rtt, 5*time.Second)
	assert.Equal(t, "noise3", cli.SuiteOf(srv))
}

func TestANodeBehindNetemSeesTheDelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	w := NewWorld(t.TempDir())
	defer w.Close()
	srv, err := w.Add(NodeSpec{Name: "srv", Server: true, Listen: true, Netem: &behaviour.NetemConfig{Delay: 20 * time.Millisecond, Seed: 1}})
	require.NoError(t, err)
	cli, err := w.Add(NodeSpec{Name: "cli"})
	require.NoError(t, err)
	require.NoError(t, w.StartAll(ctx))
	require.NotNil(t, srv.Netem)
	_, sess, err := cli.Dial(ctx, srv)
	require.NoError(t, err)
	rtt, err := sess.Ping(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, rtt, 35*time.Millisecond, "20 ms each way")
	assert.Positive(t, srv.Netem.Stats().Forwarded)
}

func TestClosingTheWorldStopsEverything(t *testing.T) {
	w := NewWorld(t.TempDir())
	_, err := w.Add(NodeSpec{Name: "a", Server: true, Listen: true})
	require.NoError(t, err)
	require.NoError(t, w.StartAll(context.Background()))
	w.Close()
	w.Close() // twice is fine
}
