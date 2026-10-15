package node

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	peerA = strings.Repeat("ab", 32)
	peerB = strings.Repeat("0c", 32)
)

func TestParseRelayAddr(t *testing.T) {
	for _, in := range []string{
		"echofog://relay/1.2.3.4:4001/" + peerA,
		"ECHOFOG://relay/1.2.3.4:4001/" + strings.ToUpper(peerA) + "/",
		"  1.2.3.4:4001/" + peerA + " ",
	} {
		a, id, err := ParseRelayAddr(in)
		require.NoError(t, err, in)
		assert.Equal(t, "1.2.3.4:4001", a)
		assert.Equal(t, peerA, id.String())
	}
	a, _, err := ParseRelayAddr("wss://relay.example.org/echofog/" + peerA)
	require.NoError(t, err)
	assert.Equal(t, "wss://relay.example.org/echofog", a)

	for _, bad := range []string{"", peerA, "1.2.3.4:4001", "1.2.3.4/" + peerA, "1.2.3.4:0/" + peerA, ":4001/" + peerA, "1.2.3.4:4001/abc"} {
		_, _, err := ParseRelayAddr(bad)
		assert.Error(t, err, bad)
	}
	assert.Equal(t, "echofog://relay/1.2.3.4:4001/"+peerA, MakeRelayLink("1.2.3.4:4001/"+peerA))
}

func testAPI(t *testing.T) *NodeApi {
	cfg := config.DefaultConfig("t")
	cfg.Storage.DatabasePath = filepath.Join(t.TempDir(), "db")
	return &NodeApi{node: &node.Node{Cfg: cfg}}
}

func TestRelaySettings_PersistAndReload(t *testing.T) {
	api := testAPI(t)
	cur, err := api.GetRelaySettings()
	require.NoError(t, err)
	assert.True(t, cur.Server, "the desktop default is a public relay")
	assert.False(t, cur.Private)

	want := RelaySettingsDTO{
		Server: true, Private: true,
		AllowedPeers: []string{strings.ToUpper(peerA), peerA, " " + peerB},
		Relays:       []string{"echofog://relay/5.6.7.8:443/" + peerB, "5.6.7.8:443/" + peerB},
		ListenPort:   4001,
	}
	require.NoError(t, api.SetRelaySettings(want))
	got, _ := api.GetRelaySettings()
	assert.Equal(t, []string{peerA, peerB}, got.AllowedPeers, "lower-cased, trimmed, no duplicates")
	assert.Equal(t, []string{"5.6.7.8:443/" + peerB}, got.Relays, "links stored as addresses, once")
	assert.Equal(t, 4001, got.ListenPort)

	cfg := api.node.Cfg
	assert.True(t, cfg.Relay.Server.Enabled && cfg.Relay.Server.Private)
	assert.Equal(t, []string{peerA, peerB}, cfg.Relay.Server.AllowedPeers)
	assert.Equal(t, ":4001", cfg.Network.ListenAddr)

	// the connection settings are saved next to it without losing the relay part
	s, _ := api.GetSettings()
	s.SocketBufferMB = 32
	require.NoError(t, api.SetSettings(s))

	fresh := config.DefaultConfig("t")
	fresh.Storage.DatabasePath = cfg.Storage.DatabasePath
	_, chosen := loadPersistedSettings(fresh)
	assert.True(t, chosen)
	assert.Equal(t, got, relaySettingsFromConfig(fresh))
	assert.Equal(t, 32, fresh.Network.SocketBufferMB)

	untouched := config.DefaultConfig("t")
	untouched.Storage.DatabasePath = filepath.Join(t.TempDir(), "db")
	_, chosen = loadPersistedSettings(untouched)
	assert.False(t, chosen, "no file: the defaults, and the UI is told so")
}

func TestRelaySettings_Rejects(t *testing.T) {
	api := testAPI(t)
	assert.Error(t, api.SetRelaySettings(RelaySettingsDTO{AllowedPeers: []string{"not-a-peer"}}))
	assert.Error(t, api.SetRelaySettings(RelaySettingsDTO{Relays: []string{"1.2.3.4:4001"}}), "an address needs the peer id")
	assert.Error(t, api.SetRelaySettings(RelaySettingsDTO{ListenPort: 70000}))
	assert.Error(t, api.AddRelay("nonsense"))

	require.NoError(t, api.AddRelay("echofog://relay/9.9.9.9:4001/"+peerA))
	got, _ := api.GetRelaySettings()
	assert.Equal(t, []string{"9.9.9.9:4001/" + peerA}, got.Relays)
}

func TestExitFreePeers_Persist(t *testing.T) {
	api := testAPI(t)
	assert.Error(t, api.SetExitFreePeers([]string{"xyz"}))
	require.NoError(t, api.SetExitFreePeers([]string{peerB, strings.ToUpper(peerB), peerA}))
	assert.Equal(t, []string{peerB, peerA}, api.GetExitFreePeers())

	fresh := config.DefaultConfig("t")
	fresh.Storage.DatabasePath = api.node.Cfg.Storage.DatabasePath
	saved, _ := loadPersistedSettings(fresh)
	assert.Equal(t, []string{peerB, peerA}, saved.ExitFreePeers)
}
