package node

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/stretchr/testify/require"
)

// persistedProtocol reads the mode the settings file holds for the next start.
func persistedProtocol(t *testing.T, api *NodeApi) int {
	t.Helper()
	raw, err := os.ReadFile(settingsPath(api.node.Cfg))
	require.NoError(t, err)
	var p persistedSettings
	require.NoError(t, json.Unmarshal(raw, &p))
	return p.Protocol
}

// AD14: the mode applies from the next start. A running v1 node keeps serving v1 after the user
// chooses 2 for the next start: GetProtocol and v2Unavailable report the mode it started with.
func TestSetProtocolKeepsRunningV1Node(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)

	require.NoError(t, api.SetProtocol(config.ProtocolV2))

	require.Equal(t, config.ProtocolV1, api.GetProtocol(), "the running node stays v1")
	require.NoError(t, api.v2Unavailable(), "v1 network methods keep working")
	require.Equal(t, config.ProtocolV2, persistedProtocol(t, api), "the choice is saved for the next start")
}

// AD14 in the other direction: a running v2 node keeps answering "v2 not built" after the user
// chooses 1, and a network method without an error result stays the zero value instead of
// touching the nil v1 swarm.
func TestSetProtocolKeepsRunningV2Node(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	require.ErrorIs(t, api.v2Unavailable(), ErrV2NotBuilt)

	require.NoError(t, api.SetProtocol(config.ProtocolV1))

	require.Equal(t, config.ProtocolV2, api.GetProtocol(), "the running node stays v2")
	require.ErrorIs(t, api.v2Unavailable(), ErrV2NotBuilt, "v2 network methods still refuse")
	require.Nil(t, api.GetPeers(), "no panic on the nil v1 swarm")
	require.Equal(t, config.ProtocolV1, persistedProtocol(t, api), "the choice is saved for the next start")
}

// Saving ordinary settings after SetProtocol must not drop the mode chosen for the next start.
func TestSetProtocolSurvivesSettingsSave(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	require.NoError(t, api.SetProtocol(config.ProtocolV2))

	s, err := api.GetSettings()
	require.NoError(t, err)
	s.SocketBufferMB = 42
	require.NoError(t, api.SetSettings(s))

	require.Equal(t, config.ProtocolV2, persistedProtocol(t, api))
	require.Equal(t, config.ProtocolV1, api.GetProtocol(), "the running node is still v1")
}
