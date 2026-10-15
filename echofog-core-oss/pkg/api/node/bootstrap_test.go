package node

import (
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBootstrapNodes_AddPersistRemove(t *testing.T) {
	cfg := config.DefaultConfig("t")
	cfg.Storage.DatabasePath = filepath.Join(t.TempDir(), "db")
	builtIn := "seed.example.org:4242/" + peerB
	cfg.Network.BootstrapNodes = []string{builtIn}
	api := NewNodeApi(&node.Node{Cfg: cfg})

	require.NoError(t, api.AddBootstrapNode("echofog://relay/1.2.3.4:4001/"+peerA))
	require.NoError(t, api.AddBootstrapNode(" 1.2.3.4:4001/"+peerA+" "), "the same node again is not an error")
	st, err := api.GetBootstrapNodes()
	require.NoError(t, err)
	require.Len(t, st.Custom, 1, "stored once")
	assert.Equal(t, "1.2.3.4:4001", st.Custom[0].Addr)
	assert.Equal(t, peerA, st.Custom[0].PeerID)
	assert.Equal(t, "echofog://relay/1.2.3.4:4001/"+peerA, st.Custom[0].Link)
	require.Len(t, st.BuiltIn, 1)
	assert.Equal(t, []string{builtIn, "1.2.3.4:4001/" + peerA}, cfg.Network.BootstrapNodes, "dialed at start and on rejoin")

	// a restart reads the saved list back
	cfg2 := config.DefaultConfig("t")
	cfg2.Storage.DatabasePath = cfg.Storage.DatabasePath
	cfg2.Network.BootstrapNodes = []string{builtIn}
	api2 := NewNodeApi(&node.Node{Cfg: cfg2})
	assert.Equal(t, []string{builtIn, "1.2.3.4:4001/" + peerA}, cfg2.Network.BootstrapNodes)

	require.NoError(t, api2.RemoveBootstrapNode("echofog://relay/1.2.3.4:4001/"+peerA))
	st, _ = api2.GetBootstrapNodes()
	assert.Empty(t, st.Custom)
	assert.Equal(t, []string{builtIn}, cfg2.Network.BootstrapNodes, "built-in nodes stay")
}

func TestBootstrapNodes_Rejects(t *testing.T) {
	api := testAPI(t)
	for _, bad := range []string{"", "1.2.3.4:4001", "1.2.3.4/" + peerA, "1.2.3.4:99999/" + peerA, "1.2.3.4:4001/xyz"} {
		assert.Error(t, api.AddBootstrapNode(bad), bad)
	}
}
