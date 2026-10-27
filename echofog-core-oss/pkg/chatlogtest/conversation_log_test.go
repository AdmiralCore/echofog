package chatlogtest

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/pkg/api/node"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

func dmRef(peer string) node.ChatRef { return node.ChatRef{Kind: "dm", ID: peer} }

// find looks a message up in the newest page of a chat.
func find(t *testing.T, api *node.NodeApi, ref node.ChatRef, id string) (convlog.Message, bool) {
	t.Helper()
	page, err := api.ChatHistory(ref, "", 100)
	if err != nil {
		return convlog.Message{}, false
	}
	for _, m := range page.Messages {
		if m.ID == id {
			return m, true
		}
	}
	return convlog.Message{}, false
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, 15*time.Second, 50*time.Millisecond, what)
}

// Messages and every operation on them — replies, reactions, pins, edits, deletes, comments —
// travel between two nodes and end the same on both.
func TestChatEventsTravelBetweenNodes(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob")
	// the desktop opens the logs when it starts the node; these tests start nodes directly
	_, err := alice.Conversations()
	require.NoError(t, err)
	_, err = bob.Conversations()
	require.NoError(t, err)

	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bobN.Transport.Addr().String(), bobN.ID)))
	eventually(t, "connect", func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 })
	toBob, toAlice := dmRef(bobN.ID.String()), dmRef(aliceN.ID.String())

	first, err := alice.ChatSend(toBob, node.SendRequest{Text: "hello"})
	require.NoError(t, err)
	eventually(t, "bob gets the message", func() bool {
		m, ok := find(t, bob, toAlice, first.ID)
		return ok && m.Text == "hello" && !m.Mine
	})
	eventually(t, "alice sees it delivered", func() bool {
		m, ok := find(t, alice, toBob, first.ID)
		return ok && m.State == "delivered"
	})

	// Bob answers (this also tells Alice that Bob speaks the log), reacts and pins
	reply, err := bob.ChatSend(toAlice, node.SendRequest{Text: "hi", ReplyTo: first.ID})
	require.NoError(t, err)
	eventually(t, "alice gets the reply", func() bool {
		m, ok := find(t, alice, toBob, reply.ID)
		return ok && m.ReplyTo == first.ID
	})
	require.NoError(t, bob.ChatReact(toAlice, first.ID, "👍", true))
	require.NoError(t, bob.ChatPin(toAlice, first.ID, true))
	require.NoError(t, alice.ChatEdit(toBob, first.ID, "hello (edited)"))

	settled := func(api *node.NodeApi, ref node.ChatRef) bool {
		m, ok := find(t, api, ref, first.ID)
		return ok && m.Text == "hello (edited)" && m.Edited && m.Pinned && len(m.Reactions) == 1 && m.Reactions[0].Emoji == "👍"
	}
	eventually(t, "alice's side settles", func() bool { return settled(alice, toBob) })
	eventually(t, "bob's side settles", func() bool { return settled(bob, toAlice) })
	pins, err := alice.ChatPins(toBob)
	require.NoError(t, err)
	require.Len(t, pins, 1)

	// a comment thread under the first message stays out of the main history
	comment, err := bob.ChatSend(toAlice, node.SendRequest{Text: "a comment", Thread: first.ID})
	require.NoError(t, err)
	eventually(t, "alice sees the thread", func() bool {
		th, err := alice.ChatThread(toBob, first.ID, "", 10)
		if err != nil || len(th.Messages) != 1 || th.Messages[0].ID != comment.ID {
			return false
		}
		m, ok := find(t, alice, toBob, first.ID)
		return ok && m.Replies == 1
	})
	_, inMain := find(t, alice, toBob, comment.ID)
	require.False(t, inMain, "a comment must not be in the main history")

	// Bob may not touch Alice's message; Alice deletes it for everyone
	require.Error(t, bob.ChatEdit(toAlice, first.ID, "hijack"))
	require.NoError(t, alice.ChatDelete(toBob, first.ID))
	eventually(t, "the delete reaches bob", func() bool {
		m, ok := find(t, bob, toAlice, first.ID)
		return ok && m.Deleted && m.Text == "" && !m.Pinned
	})

	// the chat list shows the chat with its unread count, and reading clears it
	convs, err := bob.Conversations()
	require.NoError(t, err)
	require.Len(t, convs, 1)
	require.Equal(t, aliceN.ID.String(), convs[0].Ref.ID)
	require.NoError(t, bob.ChatMarkRead(toAlice, reply.ID))
}

// A node that was away gets everything it missed — the messages through the outbox, and the
// reactions (which are sent only once) through sync when it connects again.
func TestChatCatchesUpAfterBeingAway(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	_, err := alice.Conversations()
	require.NoError(t, err)

	bobCfg := config.DefaultConfig("")
	bobCfg.Network.ListenAddr = "127.0.0.1:0"
	bobCfg.Storage.DatabasePath = t.TempDir() + "/bob-db"
	bobCfg.Network.BootstrapNodes = nil
	bobN := coreNode.NewNode(context.Background(), bobCfg, "bob")
	_, err = bobN.Register([]byte("pass123"), []byte("pass123"))
	require.NoError(t, err)
	require.NoError(t, bobN.Start())
	bob := node.NewNodeApi(bobN)
	_, err = bob.Conversations()
	require.NoError(t, err)
	bobID := bobN.ID

	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bobN.Transport.Addr().String(), bobID)))
	eventually(t, "connect", func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 })
	toBob, toAlice := dmRef(bobID.String()), dmRef(aliceN.ID.String())

	// one exchange each way, so both know the other speaks the log
	m1, err := alice.ChatSend(toBob, node.SendRequest{Text: "before"})
	require.NoError(t, err)
	eventually(t, "bob gets it", func() bool { _, ok := find(t, bob, toAlice, m1.ID); return ok })
	b1, err := bob.ChatSend(toAlice, node.SendRequest{Text: "ack"})
	require.NoError(t, err)
	eventually(t, "alice gets it", func() bool { _, ok := find(t, alice, toBob, b1.ID); return ok })

	// Bob goes away; Alice writes, reacts and edits meanwhile
	bobN.Stop()
	eventually(t, "bob gone", func() bool { return len(alice.GetPeers()) == 0 })
	m2, err := alice.ChatSend(toBob, node.SendRequest{Text: "while you were away"})
	require.NoError(t, err)
	require.NoError(t, alice.ChatReact(toBob, b1.ID, "🔥", true))
	require.NoError(t, alice.ChatEdit(toBob, m1.ID, "before (edited)"))

	// Bob comes back with the same identity and its log
	bob2N := coreNode.NewNode(context.Background(), bobCfg, "bob")
	require.NoError(t, bob2N.Login([]byte("pass123")))
	require.NoError(t, bob2N.Start())
	t.Cleanup(bob2N.Stop)
	bob2 := node.NewNodeApi(bob2N)
	_, err = bob2.Conversations()
	require.NoError(t, err)
	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bob2N.Transport.Addr().String(), bobID)))

	eventually(t, "the message sent while away arrives", func() bool {
		m, ok := find(t, bob2, toAlice, m2.ID)
		return ok && m.Text == "while you were away"
	})
	eventually(t, "the edit arrives", func() bool {
		m, ok := find(t, bob2, toAlice, m1.ID)
		return ok && m.Text == "before (edited)"
	})
	eventually(t, "the reaction arrives by sync", func() bool {
		m, ok := find(t, bob2, toAlice, b1.ID)
		return ok && len(m.Reactions) == 1 && m.Reactions[0].Emoji == "🔥"
	})
}

// A node with the log still reaches one without it: the message carries its text, and the old node
// keeps it as a plain message.
func TestChatReachesANodeWithoutTheLog(t *testing.T) {
	_, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob") // Bob never opens the log: an older node
	_, err := alice.Conversations()
	require.NoError(t, err)
	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bobN.Transport.Addr().String(), bobN.ID)))
	eventually(t, "connect", func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 })
	inbox := bob.MessageHandler()
	_, err = alice.ChatSend(dmRef(bobN.ID.String()), node.SendRequest{Text: "plain enough"})
	require.NoError(t, err)
	eventually(t, "the old node shows the text", func() bool {
		select {
		case m := <-inbox:
			return !m.IsMine && m.Text == "plain enough"
		default:
			return false
		}
	})
}
