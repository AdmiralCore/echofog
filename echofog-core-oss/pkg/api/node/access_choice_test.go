package node

import (
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Chosen on the welcome screen, before the node first starts, the mode applies to that start.
func TestAccessModeChosenBeforeStartAppliesAtOnce(t *testing.T) {
	api := testAPI(t)
	require.Nil(t, api.node.Swarm, "not started")
	require.NoError(t, api.SetAccessMode(config.AccessHidden))
	assert.Equal(t, config.AccessHidden, api.node.Cfg.Access.Mode)
	assert.Equal(t, config.AccessHidden, api.node.AccessMode())
	assert.Error(t, api.SetAccessMode("isolated"), "the app offers public and hidden only")
}
