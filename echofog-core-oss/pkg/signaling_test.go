package pkg

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/api/types"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startNodeWith(t *testing.T, name string, tune func(*config.AppConfig)) (*coreNode.Node, *node.NodeApi) {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = t.TempDir() + "/db"
	cfg.Network.BootstrapNodes = nil
	if tune != nil {
		tune(cfg)
	}
	n := coreNode.NewNode(context.Background(), cfg, name)
	_, err := n.Register([]byte("pass123"), []byte("pass123"))
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(n.Stop)
	return n, node.NewNodeApi(n)
}

// A signaling node passes on hole punching messages between peers that only it connects, and
// nothing else: no circuits for traffic, no relayed messages. Peers learn from the handshake that
// it is no relay and do not send it their traffic.
func TestSignalingNodeHelpsHolePunchingButRelaysNoTraffic(t *testing.T) {
	sN, s := startNodeWith(t, "signal", func(c *config.AppConfig) {
		c.Relay.Server.Enabled = true
		c.Relay.Server.SignalingOnly = true
		c.Network.NoAutoDial = true
	})
	// alice and bob stand for peers behind NATs: on one machine they would find each other
	noAutoDial := func(c *config.AppConfig) { c.Network.NoAutoDial = true }
	aN, a := startNodeWith(t, "alice", noAutoDial)
	bN, b := startNodeWith(t, "bob", noAutoDial)
	join := network.JoinPeerAddr(sN.Transport.Addr().String(), sN.ID)
	require.NoError(t, a.ConnectToPeer(join))
	require.NoError(t, b.ConnectToPeer(join))
	require.Eventually(t, func() bool { return len(s.GetPeers()) >= 2 && len(a.GetPeers()) > 0 && len(b.GetPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)
	require.Nil(t, aN.Swarm.GetPeer(bN.ID), "alice and bob must only meet through the signaling node")

	// what the peers learned from the handshake
	viaA := aN.Swarm.GetPeer(sN.ID)
	require.NotNil(t, viaA)
	assert.True(t, viaA.Caps().Has(handshake.CapSignal))
	assert.False(t, viaA.Relays(), "a signaling node does not announce itself as a relay")
	assert.True(t, viaA.Signals())
	assert.True(t, sN.Swarm.GetPeer(aN.ID).Relays(), "an ordinary node relays")

	// a hole punching message gets through
	var punches, others atomic.Int32
	bN.RelayClient.AddDataListener(func(_ uint32, mt net_types.MessageType, _ []byte, from types.PeerID) {
		if from != aN.ID {
			return
		}
		if mt == net_types.TypeHolePunch {
			punches.Add(1)
		} else {
			others.Add(1)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, aN.RelayClient.SendSignal(ctx, bN.ID, net_types.TypeHolePunch, []byte("punch")))
	if !assert.Eventually(t, func() bool { return punches.Load() == 1 }, 5*time.Second, 20*time.Millisecond) {
		t.Fatalf("punches %d, others %d", punches.Load(), others.Load())
	}

	// traffic does not: no circuit for it, and nothing smuggled through the signaling circuit
	assert.Error(t, aN.RelayClient.SendUniversal(ctx, bN.ID, net_types.TypeCircuitData, []byte("traffic")), "a circuit for traffic was granted")
	require.NoError(t, aN.RelayClient.SendSignal(ctx, bN.ID, net_types.TypeChatMessage, []byte("smuggled")))
	time.Sleep(300 * time.Millisecond)
	assert.Zero(t, others.Load(), "the signaling circuit carried traffic")

	st := sN.RelayServer.Stats()
	assert.True(t, st.SignalingOnly)
	assert.Equal(t, uint64(1), st.SignalRefused)
	assert.Zero(t, st.Forwarded+st.Stored)
}
