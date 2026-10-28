package chatlogtest

import (
	"testing"

	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/stretchr/testify/require"
)

// A space's channels between two nodes: the owner makes a text channel and a voice room, a member
// writes in the channel, and the message stays in that channel's history, out of the main one.
func TestSpaceChannelsTravel(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	_, bob := startNode(t, "bob")
	for _, a := range []*node.NodeApi{alice, bob} {
		_, err := a.Conversations()
		require.NoError(t, err)
	}
	require.NoError(t, bob.ConnectToPeer(joinOf(aliceN)))
	eventually(t, "connected", func() bool { return len(alice.GetPeers()) == 1 })

	g, err := alice.CreateGroup("Space", "", "group")
	require.NoError(t, err)
	invite, err := alice.GetGroupInvite(g.ID)
	require.NoError(t, err)
	_, err = bob.JoinGroup(invite)
	require.NoError(t, err)
	eventually(t, "both members", func() bool {
		m, err := bob.ListGroupMembers(g.ID)
		return err == nil && len(m) == 2
	})
	ref := groupRef(g.ID)
	eventually(t, "the group runs on the log", func() bool { return upgraded(t, alice, g.ID) && upgraded(t, bob, g.ID) })

	dev, err := alice.ChatChannelSave(ref, node.ChannelSpec{Name: "dev"})
	require.NoError(t, err)
	_, err = alice.ChatChannelSave(ref, node.ChannelSpec{Name: "Lounge", Voice: true})
	require.NoError(t, err)
	_, err = bob.ChatChannelSave(ref, node.ChannelSpec{Name: "mine"})
	require.Error(t, err, "a member does not make channels")
	eventually(t, "bob sees both channels", func() bool {
		chans, err := bob.ChatChannels(ref)
		return err == nil && len(chans) == 2
	})

	inDev := ref
	inDev.Channel = dev.ID
	m, err := bob.ChatSend(inDev, node.SendRequest{Text: "in dev"})
	require.NoError(t, err)
	eventually(t, "alice has it in dev", func() bool {
		page, err := alice.ChatHistory(inDev, "", 20)
		return err == nil && len(page.Messages) == 1 && page.Messages[0].ID == m.ID
	})
	main, err := alice.ChatHistory(ref, "", 50)
	require.NoError(t, err)
	for _, x := range main.Messages {
		require.NotEqual(t, m.ID, x.ID, "a channel's message is not in the main chat")
	}
}
