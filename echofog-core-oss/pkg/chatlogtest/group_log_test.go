package chatlogtest

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/stretchr/testify/require"
)

func groupRef(id string) node.ChatRef { return node.ChatRef{Kind: "group", ID: id} }

func upgraded(t *testing.T, api *node.NodeApi, group string) bool {
	t.Helper()
	convs, err := api.Conversations()
	if err != nil {
		return false
	}
	for _, c := range convs {
		if c.Ref.Kind == "group" && c.Ref.ID == group {
			return c.Upgraded
		}
	}
	return false
}

// A group whose members all run the log gets everything a direct chat has: replies, threads,
// reactions, pins (admins only), edits and deletes (authors only), on every member's device. A
// removed member gets nothing said after it left, and what it said before stays: a newcomer who
// joins later gets it with the rest of the history.
func TestGroupChatRunsOnTheLog(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	_, bob := startNode(t, "bob")
	carolN, carol := startNode(t, "carol")
	everybody := []*node.NodeApi{alice, bob, carol}
	for _, a := range everybody {
		_, err := a.Conversations() // the desktop opens the logs when it starts the node
		require.NoError(t, err)
	}
	require.NoError(t, bob.ConnectToPeer(joinOf(aliceN)))
	require.NoError(t, carol.ConnectToPeer(joinOf(aliceN)))
	eventually(t, "the hub", func() bool { return len(alice.GetPeers()) == 2 })

	g, err := alice.CreateGroup("Crew", "", "group")
	require.NoError(t, err)
	invite, err := alice.GetGroupInvite(g.ID)
	require.NoError(t, err)
	_, err = bob.JoinGroup(invite)
	require.NoError(t, err)
	_, err = carol.JoinGroup(invite)
	require.NoError(t, err)
	eventually(t, "all three know all three", func() bool {
		for _, a := range everybody {
			if m, err := a.ListGroupMembers(g.ID); err != nil || len(m) != 3 {
				return false
			}
		}
		return true
	})
	// a few plain messages: the members learn each other's version on the way
	for _, a := range everybody {
		_, err := a.SendGroupMessage(g.ID, "hi")
		require.NoError(t, err)
	}
	eventually(t, "the group runs on the log for everybody", func() bool {
		for _, a := range everybody {
			if !upgraded(t, a, g.ID) {
				return false
			}
		}
		return true
	})

	ref := groupRef(g.ID)
	post, err := alice.ChatSend(ref, node.SendRequest{Text: "on the log"})
	require.NoError(t, err)
	require.False(t, post.Legacy)
	for _, a := range []*node.NodeApi{bob, carol} {
		eventually(t, "the post reaches every member", func() bool { _, ok := find(t, a, ref, post.ID); return ok })
	}

	require.NoError(t, bob.ChatReact(ref, post.ID, "👍", true))
	require.NoError(t, alice.ChatPin(ref, post.ID, true))
	require.Error(t, carol.ChatPin(ref, post.ID, true), "a plain member does not pin")
	require.NoError(t, alice.ChatEdit(ref, post.ID, "on the log (edited)"))
	require.Error(t, carol.ChatDelete(ref, post.ID), "a member does not delete somebody else's message")
	comment, err := bob.ChatSend(ref, node.SendRequest{Text: "a comment", Thread: post.ID})
	require.NoError(t, err)

	settled := func(a *node.NodeApi) bool {
		m, ok := find(t, a, ref, post.ID)
		if !ok || m.Text != "on the log (edited)" || !m.Pinned || len(m.Reactions) != 1 || m.Replies != 1 {
			return false
		}
		th, err := a.ChatThread(ref, post.ID, "", 10)
		return err == nil && len(th.Messages) == 1 && th.Messages[0].ID == comment.ID
	}
	for _, a := range everybody {
		eventually(t, "every member ends with the same message", func() bool { return settled(a) })
	}

	said, err := carol.ChatSend(ref, node.SendRequest{Text: "carol was here"})
	require.NoError(t, err)
	eventually(t, "alice has carol's message", func() bool { _, ok := find(t, alice, ref, said.ID); return ok })

	// Carol is removed; what is said after that does not reach her
	require.NoError(t, alice.RemoveGroupMember(g.ID, carolN.ID.String(), false))
	eventually(t, "bob sees carol leave", func() bool {
		m, err := bob.ListGroupMembers(g.ID)
		return err == nil && len(m) == 2
	})
	after, err := alice.ChatSend(ref, node.SendRequest{Text: "after carol"})
	require.NoError(t, err)
	eventually(t, "bob gets it", func() bool { _, ok := find(t, bob, ref, after.ID); return ok })
	time.Sleep(2 * time.Second)
	page, _ := carol.ChatHistory(ref, "", 50)
	for _, m := range page.Messages {
		require.NotEqual(t, after.ID, m.ID, "a removed member got a message said after it left")
	}

	// Dave joins after carol left and gets the history, carol's message in it
	_, dave := startNode(t, "dave")
	_, err = dave.Conversations()
	require.NoError(t, err)
	require.NoError(t, dave.ConnectToPeer(joinOf(aliceN)))
	eventually(t, "dave reaches alice", func() bool { return len(dave.GetPeers()) >= 1 })
	invite, err = alice.GetGroupInvite(g.ID)
	require.NoError(t, err)
	_, err = dave.JoinGroup(invite)
	require.NoError(t, err)
	eventually(t, "dave gets the history, with what carol said before she left", func() bool {
		_, ok1 := find(t, dave, ref, said.ID)
		_, ok2 := find(t, dave, ref, after.ID)
		return ok1 && ok2
	})
	reply, err := dave.ChatSend(ref, node.SendRequest{Text: "hello from dave"})
	require.NoError(t, err)
	eventually(t, "the newcomer's message reaches the others", func() bool { _, ok := find(t, bob, ref, reply.ID); return ok })
}
