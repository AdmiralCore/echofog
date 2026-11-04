package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

func newFacadeNode(t *testing.T, protocol int) *NodeApi {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Protocol = protocol
	cfg.Storage.DatabasePath = filepath.Join(t.TempDir(), "db")
	node := coreNode.NewNode(context.Background(), cfg, "api-backends")
	t.Cleanup(node.Cancel)
	return NewNodeApi(node)
}

// Protocol 1 keeps the facade itself as every backend: no code moved, no behaviour changed.
func TestBackendsProtocol1IsTheFacade(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	require.Same(t, api, api.backends.net)
	require.Same(t, api, api.backends.contact)
	require.Same(t, api, api.backends.chat)
	require.Same(t, api, api.backends.group)
	require.Same(t, api, api.backends.file)
	require.Same(t, api, api.backends.call)
	require.Same(t, api, api.backends.vpn)
}

// Protocol 2 selects the not-built backends, whose network methods answer the agreed message.
func TestBackendsProtocol2AnswerNotBuilt(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	require.IsType(t, v2PendingBackend{}, api.backends.net)
	require.IsType(t, v2PendingBackend{}, api.backends.chat)
	require.IsType(t, v2PendingBackend{}, api.backends.vpn)

	err := api.backends.net.ConnectToPeer("127.0.0.1:1/deadbeef")
	require.ErrorIs(t, err, ErrV2NotBuilt)
	require.EqualError(t, err, "v2 ещё не собран")

	require.Nil(t, api.backends.net.GetPeers(), "a method without an error result returns the zero value")
}

// An unknown value in the config means v1: a half mode is worse than none.
func TestChooseBackendsTreatsUnknownAsV1(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	api.node.Cfg.Protocol = 9
	require.Same(t, api, chooseBackends(api.node.Cfg.ProtocolOrDefault(), api).net)
}

// Settings persist the mode for the next start; GetConfiguredConfig applies it before NewNode, so
// the desktop passes the mode into the node it builds.
func TestGetConfiguredConfigAppliesPersistedProtocol(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	profile := "p2"
	dir := filepath.Join(home, ".echofog", "profiles", profile)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ui_settings.json"), []byte(`{"protocol":2}`), 0o600))

	cfg := GetConfiguredConfig(profile)
	require.Equal(t, config.ProtocolV2, cfg.Protocol)
	require.True(t, cfg.IsV2())
}

// Saving ordinary settings must not drop the mode chosen for the next start.
func TestSaveSettingsPreservesProtocol(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig("")
	cfg.Storage.DatabasePath = filepath.Join(dir, "db")
	path := filepath.Join(dir, settingsFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"protocol":2}`), 0o600))

	settingsMu.Lock()
	err := saveSettingsLocked(cfg, func(p *persistedSettings) { p.SocketBufferMB = 7 })
	settingsMu.Unlock()
	require.NoError(t, err)

	var p persistedSettings
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &p))
	require.Equal(t, config.ProtocolV2, p.Protocol)
	require.Equal(t, 7, p.SettingsDTO.SocketBufferMB)
}

func TestGetConfiguredConfigDefaultsToV1(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := filepath.Join(home, ".echofog", "profiles", "p1")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ui_settings.json"), []byte(`{"protocol":9}`), 0o600))

	cfg := GetConfiguredConfig("p1")
	require.Equal(t, config.ProtocolV1, cfg.Protocol)
}
