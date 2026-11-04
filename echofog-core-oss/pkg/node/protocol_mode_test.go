package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

// newModeNode makes a registered node in a fresh profile. It never touches the network: the tests
// below replace startNetworkFn, the single point where the v1 network stack starts.
func newModeNode(t *testing.T, protocol int) *Node {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Protocol = protocol
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Network.TCPListenAddr = "off"
	cfg.Network.WSListenAddr = ""
	cfg.Network.BootstrapNodes = nil
	cfg.Storage.DatabasePath = filepath.Join(t.TempDir(), "db")
	n := NewNode(context.Background(), cfg, "protocol-mode")
	_, err := n.Register([]byte("pass123"), []byte(""))
	require.NoError(t, err)
	return n
}

// interceptV1Network replaces the only entry into the v1 network stack; the returned flag says
// whether the offline start reached it.
func interceptV1Network(t *testing.T) *bool {
	t.Helper()
	called := false
	prev := startNetworkFn
	startNetworkFn = func(*Node) error {
		called = true
		return nil
	}
	t.Cleanup(func() { startNetworkFn = prev })
	return &called
}

// At protocol 2 the process must not reach the v1 network at all: no transport, no swarm, no
// services. The storage, identity and wallet still come up, and the v2 data directory exists.
func TestProtocol2StartsWithoutV1Network(t *testing.T) {
	called := interceptV1Network(t)
	// Defence in depth: even a stray dial would be refused, and the start has to succeed anyway.
	network.SetDialOnly([]string{})
	t.Cleanup(func() { network.SetDialOnly(nil) })

	n := newModeNode(t, config.ProtocolV2)
	t.Cleanup(n.Stop)
	require.NoError(t, n.Start(), "the offline start must not need a dial or a listen")
	require.False(t, *called, "the v1 network stack must not be started")

	require.Nil(t, n.Transport)
	require.Nil(t, n.Net)
	require.Nil(t, n.WS)
	require.Nil(t, n.Dispatcher)
	require.Nil(t, n.Swarm)
	require.Nil(t, n.DhtEngine)
	require.Nil(t, n.Messenger)
	require.Nil(t, n.Contact)
	require.Nil(t, n.CallManager)
	require.Nil(t, n.MDNS)
	require.Nil(t, n.RelayServer)
	require.Nil(t, n.RelayClient)
	require.Nil(t, n.ExitService)
	require.Nil(t, n.Services)
	require.Nil(t, n.Files)

	require.NotNil(t, n.Storage, "storage works offline")
	require.NotNil(t, n.TonWallet, "the wallet comes up offline")
	require.NotEqual(t, types.PeerID{}, n.ID, "the identity is loaded")
	require.True(t, n.NetworkOff())

	dir, err := n.V2DataDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(filepath.Dir(n.Cfg.Storage.DatabasePath), "v2"), dir)
	st, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, st.IsDir())
}

// The switch must not change protocol 1: the v1 network start is still called.
func TestProtocol1StillStartsV1Network(t *testing.T) {
	called := interceptV1Network(t)
	n := newModeNode(t, config.ProtocolV1)
	t.Cleanup(n.Stop)
	require.NoError(t, n.Start())
	require.True(t, *called, "protocol 1 keeps starting the v1 network")
	require.False(t, n.NetworkOff())
}

// The v2 data directory is only a path and a creation; it does not depend on the network.
func TestV2DataDirIsCreatedOnDemand(t *testing.T) {
	n := newModeNode(t, config.ProtocolV2)
	t.Cleanup(n.Stop)
	dir, err := n.V2DataDir()
	require.NoError(t, err)
	require.DirExists(t, dir)
	again, err := n.V2DataDir()
	require.NoError(t, err)
	require.Equal(t, dir, again)
}
