package relayack

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/p2p/routing"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/api/types"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

func start(t *testing.T, name string) (*coreNode.Node, *node.NodeApi) {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = t.TempDir() + "/db"
	cfg.Network.BootstrapNodes = nil
	cfg.Network.NoAutoDial = true // on one machine they would find each other
	n := coreNode.NewNode(context.Background(), cfg, name)
	_, err := n.Register([]byte("pass123"), []byte("pass123"))
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(n.Stop)
	return n, node.NewNodeApi(n)
}

func connect(t *testing.T, a, b *coreNode.Node) {
	t.Helper()
	require.NoError(t, node.NewNodeApi(a).ConnectToPeer(network.JoinPeerAddr(b.Transport.Addr().String(), b.ID)))
	require.Eventually(t, func() bool { return a.Swarm.GetPeer(b.ID) != nil }, 5*time.Second, 50*time.Millisecond)
}

type answer struct {
	relay          types.PeerID
	status, reason byte
}

// meetAndLeave: the sender meets the recipient directly (so it has its keys), then the recipient
// goes away; what the sender writes next can only go through the relay.
func meetAndLeave(t *testing.T, aN *coreNode.Node, a *node.NodeApi, z *coreNode.Node) {
	t.Helper()
	connect(t, aN, z)
	zApi := node.NewNodeApi(z)
	inbox := zApi.MessageHandler()
	require.NoError(t, a.SendMessage(zApi.GetMyPubKey(), "hello"))
	require.Eventually(t, func() bool {
		select {
		case m := <-inbox:
			return m.Text == "hello"
		default:
			return false
		}
	}, 10*time.Second, 20*time.Millisecond)
	z.Stop()
	require.Eventually(t, func() bool { return aN.Swarm.GetPeer(z.ID) == nil }, 10*time.Second, 100*time.Millisecond)
}

// A relay answers every message request. One that keeps the message for an away recipient says so,
// and the message stays "relayed"; one that will not keep it (the recipient never used it) says
// so too, the message goes back to waiting for a way, and the relay is tried last for a while.
func TestRelayAnswersEveryMessageRequest(t *testing.T) {
	aN, a := start(t, "alice")
	rN, r := start(t, "relay")
	connect(t, aN, rN)

	var mu sync.Mutex
	var answers []answer
	aN.RelayClient.OnRelayAck(func(relay types.PeerID, reqID []byte, status, reason byte) {
		mu.Lock()
		answers = append(answers, answer{relay, status, reason})
		mu.Unlock()
		aN.Messenger.HandleRelayAck(relay, reqID, status == routing.RelayAckStored || status == routing.RelayAckForwarded)
	})
	last := func() (answer, int) {
		mu.Lock()
		defer mu.Unlock()
		if len(answers) == 0 {
			return answer{}, 0
		}
		return answers[len(answers)-1], len(answers)
	}
	states := func(peer types.PeerID) map[string]string { return aN.Messenger.PendingStates(peer) }

	// 1. a recipient that never used the relay: its mailbox there refuses
	zN, _ := start(t, "zed")
	meetAndLeave(t, aN, a, zN)
	require.NoError(t, a.SendMessage(node.NewNodeApi(zN).GetMyPubKey(), "to a stranger of the relay"))
	require.Eventually(t, func() bool { an, n := last(); return n > 0 && an.status == routing.RelayAckRefused }, 10*time.Second, 50*time.Millisecond)
	an, _ := last()
	require.Equal(t, rN.ID, an.relay)
	require.Equal(t, routing.RefusedMailbox, an.reason)
	require.Eventually(t, func() bool {
		for _, st := range states(zN.ID) {
			if st == messenger.StateRelayed {
				return false
			}
		}
		return len(states(zN.ID)) > 0
	}, 5*time.Second, 50*time.Millisecond, "a refused message still shows as with the relay")
	require.True(t, aN.RelayClient.IsFull(rN.ID), "the refusing relay is not tried last")

	// 2. a recipient that uses the relay and goes away: the relay keeps the message and says so
	yN, _ := start(t, "yan")
	connect(t, yN, rN)
	require.Eventually(t, func() bool { return len(r.GetPeers()) >= 2 }, 5*time.Second, 50*time.Millisecond)
	meetAndLeave(t, aN, a, yN)
	_, before := last()
	require.NoError(t, a.SendMessage(node.NewNodeApi(yN).GetMyPubKey(), "kept for you"))
	require.Eventually(t, func() bool {
		an, n := last()
		return n > before && (an.status == routing.RelayAckStored || an.status == routing.RelayAckForwarded)
	}, 10*time.Second, 50*time.Millisecond, "the relay did not say it took the message")
	require.Eventually(t, func() bool {
		for _, st := range states(yN.ID) {
			if st == messenger.StateRelayed {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)
}
