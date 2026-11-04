package exit_gateway

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
)

// resetSwarmPools releases every cached pool, so each test starts without one.
func resetSwarmPools() {
	swarmPoolsMu.Lock()
	keys := make([]swarmPoolKey, 0, len(swarmPools))
	for k := range swarmPools {
		keys = append(keys, k)
	}
	swarmPoolsMu.Unlock()
	for _, k := range keys {
		ReleaseSwarmPool(k.sw, k.exit)
	}
}

func TestSwarmOpenerIntegration(t *testing.T) {
	resetSwarmPools()
	defer resetSwarmPools()

	peerID := types.PeerID{1, 2, 3}
	cfg := config.FlowShapingConfig{
		MaxConnections:     2,
		MaxConcurrency:     4,
		MaxBytesMin:        1000,
		MaxBytesMax:        1000,
		MaxConnLifetimeMin: 5 * time.Second,
		MaxConnLifetimeMax: 5 * time.Second,
		MaxStreamsPerConn:  100,
		MinDialInterval:    10 * time.Millisecond,
	}

	// Nil swarm returns ErrNoStreamOpener
	opener := SwarmOpenerWithConfig(nil, peerID, cfg)
	_, err := opener(context.Background())
	assert.ErrorIs(t, err, ErrNoStreamOpener)
}
