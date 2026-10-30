package node

import (
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Anonymous mode from the app: Tor needs the proxy, the relay mode a pinned relay; the choice is
// saved for the next start and read back from the settings file. With the relay mode on and no
// proxy or VPN, the app makes no lookups of its own.
func TestAnonymitySettings(t *testing.T) {
	n := testAPI(t)
	require.Error(t, n.SetAnonymity(config.AnonTor, false), "no proxy yet")
	require.Error(t, n.SetAnonymity(config.AnonRelay, false), "no relay pinned yet")
	require.Error(t, n.SetAnonymity("loud", false))

	require.NoError(t, n.SetProxySettings("127.0.0.1:9150", ""))
	require.NoError(t, n.SetAnonymity(config.AnonTor, true))
	st, err := n.GetAnonymity()
	require.NoError(t, err)
	assert.Equal(t, config.AnonTor, st.Mode)
	assert.True(t, st.WalletDirect)

	cfg := config.DefaultConfig("t")
	cfg.Storage.DatabasePath = n.node.Cfg.Storage.DatabasePath
	loadPersistedSettings(cfg)
	assert.Equal(t, config.AnonTor, cfg.Network.Anonymous, "the next start reads it")
	assert.True(t, cfg.Network.AnonWalletDirect)

	n.node.Cfg.Network.Anonymous = config.AnonRelay
	assert.Nil(t, n.OwnHTTPClient(), "no lookup that would show the address")
	n.node.Cfg.Network.Anonymous = ""
	assert.NotNil(t, n.OwnHTTPClient())
}
