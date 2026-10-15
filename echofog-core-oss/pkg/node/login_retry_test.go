package node_test

import (
	"context"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

// A mistyped password must not leave the profile's database locked: the desktop builds a fresh
// node for every login attempt, so the second attempt with the right password has to open it.
func TestLoginAfterWrongPasswordCanRetry(t *testing.T) {
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = t.TempDir() + "/db"
	cfg.Network.BootstrapNodes = nil

	reg := node.NewNode(context.Background(), cfg, "reg")
	_, err := reg.Register([]byte("right-pass"), []byte(""))
	require.NoError(t, err)
	reg.Stop()

	wrong := node.NewNode(context.Background(), cfg, "wrong")
	require.Error(t, wrong.Login([]byte("typo")))

	right := node.NewNode(context.Background(), cfg, "right")
	require.NoError(t, right.Login([]byte("right-pass")), "the database must not stay locked by the failed attempt")
	right.Stop()
}
