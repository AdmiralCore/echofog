package pkg

import (
	"context"
	"os"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/api/node"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

// Logging in again to a profile made earlier: what the app does after a restart.
func TestLoginAgainAfterRestart(t *testing.T) {
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = t.TempDir() + "/db"
	cfg.Network.BootstrapNodes = nil

	first := node.NewNodeApi(coreNode.NewNode(context.Background(), cfg, "first"))
	_, err := first.Register("pass123", "pass123")
	require.NoError(t, err)
	require.NoError(t, first.StartNode())
	first.StopNode()

	again := node.NewNodeApi(coreNode.NewNode(context.Background(), cfg, "again"))
	require.NoError(t, again.Login("pass123"))
	require.NoError(t, again.StartNode())
	again.StopNode()
}

// A profile written by the version before the encrypted storage opens, and its history is there.
func TestLoginToOldProfile(t *testing.T) {
	dir := os.Getenv("OLD_DB_DIR")
	if dir == "" {
		t.Skip("set OLD_DB_DIR to profiles made by an older version")
	}
	for _, name := range []string{"alice", "bob"} {
		cfg := config.DefaultConfig("")
		cfg.Network.ListenAddr = "127.0.0.1:0"
		cfg.Storage.DatabasePath = dir + "/" + name
		cfg.Network.BootstrapNodes = nil
		api := node.NewNodeApi(coreNode.NewNode(context.Background(), cfg, name))
		require.NoError(t, api.Login("pass123"))
		require.NoError(t, api.StartNode())
		contacts, err := api.GetContacts()
		require.NoError(t, err)
		require.NotEmpty(t, contacts, name)
		msgs, err := api.GetMessages(contacts[0].PeerID, 10, 0)
		require.NoError(t, err)
		require.NotEmpty(t, msgs, name)
		api.StopNode()
	}
}
