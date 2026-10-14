package pkg

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A private relay opens circuits for the peers its operator allowed and refuses the rest.
func TestPrivateRelayServesOnlyAllowedPeers(t *testing.T) {
	// B (allowed) and C (not allowed) are the two callers; D is a common target both try to reach
	// through the relay, so neither reuses a circuit the other opened.
	bN, b := startNodeWith(t, "relay-b", nil)
	cN, c := startNodeWith(t, "relay-c", nil)
	dN, d := startNodeWith(t, "relay-d", nil)

	rN, r := startNodeWith(t, "relay", func(cfg *config.AppConfig) {
		cfg.Relay.Server.Enabled = true
		cfg.Relay.Server.Private = true
		cfg.Relay.Server.AllowedPeers = []string{bN.ID.String()} // B is allowed, C is not
	})
	require.True(t, rN.RelayServer.Stats().Private)

	join := network.JoinPeerAddr(rN.Transport.Addr().String(), rN.ID)
	require.NoError(t, b.ConnectToPeer(join))
	require.NoError(t, c.ConnectToPeer(join))
	require.NoError(t, d.ConnectToPeer(join))
	require.Eventually(t, func() bool { return len(r.GetPeers()) >= 3 }, 5*time.Second, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// B is on the allow list: the relay opens a circuit to D for it
	_, err := bN.RelayClient.OpenCircuit(ctx, rN.ID, dN.ID)
	assert.NoError(t, err, "an allowed peer should get a circuit through the private relay")

	// C is not on the allow list: the relay refuses its request to the same target
	_, err = cN.RelayClient.OpenCircuit(ctx, rN.ID, dN.ID)
	assert.Error(t, err, "a peer not on the allow list must be refused")

	assert.Positive(t, rN.RelayServer.Stats().AccessRefused, "the refusal should be counted")
}
