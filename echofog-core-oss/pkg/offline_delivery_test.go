package pkg

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/dto"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/node"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A message to a contact who is offline is not lost: it waits in the outbox, shows as pending, and
// arrives when the contact comes back.
func TestMessageToOfflineContactIsDeliveredLater(t *testing.T) {
	_, alice := startNode(t, "alice")

	bobCfg := config.DefaultConfig("")
	bobCfg.Network.ListenAddr = "127.0.0.1:0"
	bobCfg.Storage.DatabasePath = t.TempDir() + "/bob-db"
	bobCfg.Network.BootstrapNodes = nil
	bobN := coreNode.NewNode(context.Background(), bobCfg, "bob")
	_, err := bobN.Register([]byte("pass123"), []byte("pass123"))
	require.NoError(t, err)
	require.NoError(t, bobN.Start())
	bob := node.NewNodeApi(bobN)
	bobPub, bobID := bob.GetMyPubKey(), bobN.ID

	// they meet once, so that the key bundles are known
	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bobN.Transport.Addr().String(), bobID)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)
	bobInbox := bob.MessageHandler()
	require.NoError(t, alice.SendMessage(bobPub, "hello while online"))
	require.Eventually(t, func() bool {
		select {
		case m := <-bobInbox:
			return m.Text == "hello while online"
		default:
			return false
		}
	}, 5*time.Second, 20*time.Millisecond)

	// Bob goes away
	bobN.Stop()
	require.Eventually(t, func() bool { return len(alice.GetPeers()) == 0 }, 10*time.Second, 100*time.Millisecond)

	require.NoError(t, alice.SendMessage(bobPub, "while you were away"), "sending to an offline contact must not fail")
	msgs, err := alice.GetMessages(bobID.String(), 10, 0)
	require.NoError(t, err)
	var pending bool
	for _, m := range msgs {
		if m.Text == "while you were away" {
			pending = m.Pending
		}
	}
	assert.True(t, pending, "the message must be marked pending")

	// Bob returns with the same identity
	bob2N := coreNode.NewNode(context.Background(), bobCfg, "bob")
	require.NoError(t, bob2N.Login([]byte("pass123")))
	require.NoError(t, bob2N.Start())
	t.Cleanup(bob2N.Stop)
	bob2 := node.NewNodeApi(bob2N)
	inbox := bob2.MessageHandler()

	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bob2N.Transport.Addr().String(), bobID)))

	require.Eventually(t, func() bool {
		select {
		case m := <-inbox:
			return !m.IsMine && m.Text == "while you were away"
		default:
			return false
		}
	}, 15*time.Second, 50*time.Millisecond, "the queued message never arrived")

	require.Eventually(t, func() bool {
		msgs, _ := alice.GetMessages(bobID.String(), 10, 0)
		for _, m := range msgs {
			if m.Text == "while you were away" {
				return !m.Pending
			}
		}
		return false
	}, 5*time.Second, 100*time.Millisecond, "the message stays pending after delivery")
}

// If one side loses its encryption session (or both start one at the same moment and end up with
// different ones), nothing arrives any more until somebody notices. The side that cannot read says
// so, both drop the session, and what was waiting is encrypted again.
func TestBrokenSessionHeals(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob")
	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bobN.Transport.Addr().String(), bobN.ID)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)

	bobInbox := bob.MessageHandler()
	aliceInbox := alice.MessageHandler()
	wait := func(inbox <-chan *dto.MessageDTO, text string) bool {
		deadline := time.After(25 * time.Second)
		for {
			select {
			case m := <-inbox:
				if !m.IsMine && m.Text == text {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}

	require.NoError(t, alice.SendMessage(bob.GetMyPubKey(), "before"))
	require.True(t, wait(bobInbox, "before"))

	// Bob loses his session: Alice's next message is garbage to him
	require.NoError(t, bob.ResetEncryptionSession(aliceN.ID.String()))
	require.NoError(t, alice.SendMessage(bob.GetMyPubKey(), "after the loss"))
	assert.True(t, wait(bobInbox, "after the loss"), "the message must arrive after the sessions were reset")

	// and the conversation works in both directions again
	require.NoError(t, bob.SendMessage(alice.GetMyPubKey(), "and back"))
	assert.True(t, wait(aliceInbox, "and back"))
	require.NoError(t, alice.SendMessage(bob.GetMyPubKey(), "once more"))
	assert.True(t, wait(bobInbox, "once more"))
}
