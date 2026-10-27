package chatlogtest

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/node"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

func startAt(t *testing.T, name, db string, tune func(*config.AppConfig)) (*coreNode.Node, *node.NodeApi) {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Storage.DatabasePath = db
	cfg.Network.BootstrapNodes = nil
	cfg.Network.NoAutoDial = true // the topology of the test, not one the mesh repairs
	if tune != nil {
		tune(cfg)
	}
	n := coreNode.NewNode(context.Background(), cfg, name)
	if n.IsRegistered() {
		require.NoError(t, n.Login([]byte("pass123")))
	} else {
		_, err := n.Register([]byte("pass123"), []byte("pass123"))
		require.NoError(t, err)
	}
	require.NoError(t, n.Start())
	return n, node.NewNodeApi(n)
}

func joinOf(n *coreNode.Node) string { return network.JoinPeerAddr(n.Transport.Addr().String(), n.ID) }

// A message to a contact who is away reaches it through the relay the contact named for its mail,
// although the sender shares no relay with it (audit finding F1).
func TestMailGoesToTheRecipientsMailboxRelay(t *testing.T) {
	base := t.TempDir()
	relayN, _ := startAt(t, "relay", base+"/relay", func(c *config.AppConfig) {
		c.Role = config.RoleServer
		c.Relay.Server.Enabled = true
	})
	t.Cleanup(relayN.Stop)
	aliceN, alice := startAt(t, "alice", base+"/alice", nil)
	t.Cleanup(aliceN.Stop)
	bobN, bob := startAt(t, "bob", base+"/bob", nil)
	bobPub, bobID := bob.GetMyPubKey(), bobN.ID

	// Bob uses the relay; Alice and Bob meet once, so each has the other's keys
	require.NoError(t, bob.ConnectToPeer(joinOf(relayN)))
	require.NoError(t, alice.ConnectToPeer(joinOf(bobN)))
	eventually(t, "connections", func() bool {
		return bobN.Swarm.GetPeer(relayN.ID) != nil && aliceN.Swarm.GetPeer(bobID) != nil
	})
	require.NoError(t, alice.SendMessage(bobPub, "hello"))
	eventually(t, "the first message", func() bool {
		msgs, _ := bob.GetMessages(aliceN.ID.String(), 10, 0)
		return len(msgs) > 0
	})

	// Bob names the relay as his mailbox; his record reaches Alice
	bobN.RefreshMailboxes()
	require.Len(t, bobN.Swarm.MyMailboxHints(), 1)
	eventually(t, "alice learns bob's mailbox", func() bool {
		hints := aliceN.Swarm.RefreshMailboxHints(bobID)
		return len(hints) == 1 && hints[0].Peer == relayN.ID.String()
	})

	// Bob goes away; Alice is not connected to the relay and never was
	bobN.Stop()
	eventually(t, "bob gone", func() bool { return aliceN.Swarm.GetPeer(bobID) == nil })
	require.Nil(t, aliceN.Swarm.GetPeer(relayN.ID))
	require.NoError(t, alice.SendMessage(bobPub, "kept by your mailbox"))
	require.Eventually(t, func() bool {
		items, _ := relayN.MailboxManager.GetPending(bobID)
		return len(items) > 0
	}, 60*time.Second, 200*time.Millisecond, "the message never reached bob's mailbox relay")

	// Bob comes back, connected only to his relay
	bob2N, bob2 := startAt(t, "bob", base+"/bob", nil)
	t.Cleanup(bob2N.Stop)
	_, err := bob2.Conversations() // the desktop opens the log on start
	require.NoError(t, err)
	require.NoError(t, bob2.ConnectToPeer(joinOf(relayN)))
	eventually(t, "bob gets the mail kept for him", func() bool {
		msgs, _ := bob2.GetMessages(aliceN.ID.String(), 10, 0)
		for _, m := range msgs {
			if m.Text == "kept by your mailbox" {
				return true
			}
		}
		return false
	})
}
