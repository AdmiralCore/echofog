package node_test

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
)

// In guard mode a link to a user's node across the border is not made: EnsureConnected says the
// traffic goes through a relay, and nothing is dialed or punched. With guard mode off it is as it was.
func TestEnsureConnectedToAUserAcrossTheBorder(t *testing.T) {
	n := startTestNode(t, "user")
	var other types.PeerID
	other[0] = 7

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := n.EnsureConnected(ctx, other)
	assert.ErrorIs(t, err, swarm.ErrUserLinkViaRelay)

	n.Cfg.AntiDPI.GuardMode = config.GuardOff
	err = n.EnsureConnected(ctx, other)
	assert.NotErrorIs(t, err, swarm.ErrUserLinkViaRelay)
}
