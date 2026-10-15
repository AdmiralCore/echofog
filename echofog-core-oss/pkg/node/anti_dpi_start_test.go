package node

import (
	"context"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/stretchr/testify/require"
)

// fakeScan replaces the neighbour scan so tests never touch the network; found switches the result.
func fakeScan(t *testing.T, found *atomic.Bool) {
	t.Helper()
	prev, prevEvery := scanForNeighbor, mimicryRetryEvery
	scanForNeighbor = func(context.Context, netip.Addr, mimicry.ServerProfile, *slog.Logger) (mimicry.Neighbor, bool) {
		if found.Load() {
			return mimicry.Neighbor{ServerName: "neighbour.example.net", Addr: "192.0.2.10:443"}, true
		}
		return mimicry.Neighbor{}, false
	}
	mimicryRetryEvery = 50 * time.Millisecond
	t.Cleanup(func() { scanForNeighbor, mimicryRetryEvery = prev, prevEvery })
}

func publicNode(t *testing.T) *Node {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	// public as far as the anti-DPI rule is concerned; the test never binds it (see below)
	cfg.Network.TCPListenAddr = "0.0.0.0:0"
	cfg.Storage.DatabasePath = t.TempDir() + "/db"
	cfg.Network.BootstrapNodes = nil
	n := NewNode(context.Background(), cfg, "public")
	_, err := n.Register([]byte("pass123"), []byte(""))
	require.NoError(t, err)
	return n
}

// With no camouflage site the node must still run (QUIC needs none), only without the public TCP
// listener — refusing to start the whole node would drop a perfectly good UDP relay.
func TestPublicNodeWithoutNeighbourStartsOnUDPOnly(t *testing.T) {
	var found atomic.Bool
	fakeScan(t, &found)
	n := publicNode(t)
	require.NoError(t, n.Start(), "no neighbour must not stop the node")
	t.Cleanup(n.Stop)
	require.NotNil(t, n.Transport.Addr(), "QUIC is up")
	require.Nil(t, n.Net.TCP.Addr(), "the public TCP listener waits for a camouflage site")
}

// Once a neighbour turns up, the TCP listener comes on without a restart, mimicking it.
func TestRetryTurnsTCPOnWhenANeighbourIsFound(t *testing.T) {
	var found atomic.Bool
	fakeScan(t, &found)
	// The retry Start launches would listen on "0.0.0.0:0", an outside interface (a firewall
	// prompt on Windows): keep it asleep and drive one on loopback instead.
	mimicryRetryEvery = time.Hour
	n := publicNode(t)
	require.NoError(t, n.Start())
	t.Cleanup(n.Stop)
	require.Nil(t, n.Net.TCP.Addr())

	go n.retryMimicryTarget("127.0.0.1:0", 20*time.Millisecond)
	found.Store(true)
	require.Eventually(t, func() bool { return n.Net.TCP.Addr() != nil }, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, "neighbour.example.net", n.Cfg.AntiDPI.TLSServerName)
	require.Equal(t, "192.0.2.10:443", n.Cfg.AntiDPI.CamouflageAddr)
}
