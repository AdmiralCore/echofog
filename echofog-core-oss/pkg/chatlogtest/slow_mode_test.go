package chatlogtest

import (
	"testing"

	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/stretchr/testify/require"
)

// Slow mode holds a plain member back between messages, in the main channel and in a channel,
// once the setting has reached them; the owner is not held back, and comments in threads are not.
func TestSlowModeHoldsAMemberBack(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	_, bob := startNode(t, "bob")
	for _, a := range []*node.NodeApi{alice, bob} {
		_, err := a.Conversations()
		require.NoError(t, err)
	}
	require.NoError(t, bob.ConnectToPeer(joinOf(aliceN)))
	eventually(t, "connected", func() bool { return len(alice.GetPeers()) == 1 })
	g, err := alice.CreateGroup("Crew", "", "group")
	require.NoError(t, err)
	eventually(t, "the group runs on the log", func() bool { return upgraded(t, alice, g.ID) })
	invite, err := alice.GetGroupInvite(g.ID)
	require.NoError(t, err)
	_, err = bob.JoinGroup(invite)
	require.NoError(t, err)
	ref := groupRef(g.ID)
	eventually(t, "bob may write", func() bool {
		rows, err := bob.ChatRows()
		if err != nil {
			return false
		}
		for _, r := range rows {
			if r.Ref.ID == g.ID {
				return r.Can.Send && r.Pending == ""
			}
		}
		return false
	})

	require.NoError(t, alice.ChatMainChannelSave(ref, node.ChannelSpec{SlowSecs: 60}))
	dev, err := alice.ChatChannelSave(ref, node.ChannelSpec{Name: "dev", SlowSecs: 60})
	require.NoError(t, err)
	eventually(t, "the slow mode reaches bob", func() bool {
		mc, err := bob.ChatMainChannel(ref)
		if err != nil || mc.SlowSecs != 60 {
			return false
		}
		chans, err := bob.ChatChannels(ref)
		return err == nil && len(chans) == 1 && chans[0].SlowSecs == 60
	})

	for _, at := range []node.ChatRef{ref, {Kind: ref.Kind, ID: ref.ID, Channel: dev.ID}} {
		first, err := bob.ChatSend(at, node.SendRequest{Text: "one"})
		require.NoError(t, err)
		_, err = bob.ChatSend(at, node.SendRequest{Text: "two"})
		require.Error(t, err, "held back in %q", at.Channel)
		_, err = bob.ChatSend(at, node.SendRequest{Text: "a comment", Thread: first.ID})
		require.NoError(t, err, "a comment is not held back")
	}
	for i := 0; i < 2; i++ {
		_, err := alice.ChatSend(ref, node.SendRequest{Text: "owner"})
		require.NoError(t, err, "the owner is not held back")
	}
}
