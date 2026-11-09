package node

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// ECHOFOG_PROTOCOL is only the default: a profile that saved its own mode wins over it.
func TestProtocolFromEnvIsTheDefaultAndProfileOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ECHOFOG_PROTOCOL", "2")

	// no settings file: the environment decides the mode of the profile
	require.Equal(t, config.ProtocolV2, GetConfiguredConfig("env-only").Protocol)

	// a profile that saved v1 overrides the env default
	dir := filepath.Join(home, ".echofog", "profiles", "saved-v1")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ui_settings.json"), []byte(`{"protocol":1}`), 0o600))
	require.Equal(t, config.ProtocolV1, GetConfiguredConfig("saved-v1").Protocol)

	// and a saved v2 wins when the env says v1
	t.Setenv("ECHOFOG_PROTOCOL", "1")
	dir = filepath.Join(home, ".echofog", "profiles", "saved-v2")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ui_settings.json"), []byte(`{"protocol":2}`), 0o600))
	require.Equal(t, config.ProtocolV2, GetConfiguredConfig("saved-v2").Protocol)
}

// T11.7: the launch flag (--protocol 2) chooses the mode of this run only: it wins over what the profile
// saved and over ECHOFOG_PROTOCOL, and it saves nothing, so the next start without it is as before.
func TestForcedProtocolWinsForOneRunAndSavesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".echofog", "profiles", "forced")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ui_settings.json"), []byte(`{"protocol":1}`), 0o600))

	ForceProtocol(config.ProtocolV2)
	t.Cleanup(func() { ForceProtocol(0) })
	require.Equal(t, config.ProtocolV2, GetConfiguredConfig("forced").Protocol, "the flag wins over the saved v1")

	ForceProtocol(0)
	require.Equal(t, config.ProtocolV1, GetConfiguredConfig("forced").Protocol, "without the flag the profile decides")
	raw, err := os.ReadFile(filepath.Join(dir, "ui_settings.json"))
	require.NoError(t, err)
	require.JSONEq(t, `{"protocol":1}`, string(raw), "the flag saved nothing")

	// a value that is not a protocol is ignored
	ForceProtocol(7)
	require.Equal(t, config.ProtocolV1, GetConfiguredConfig("forced").Protocol)
}
