package chatlogtest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/stretchr/testify/require"
)

// A file is a message too: in an announcements channel a plain member sends neither a message nor
// a file, the old-style file path included, but still comments in a thread, with a file as well.
func TestChannelRightsHoldFilesToo(t *testing.T) {
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
	eventually(t, "bob may write", func() bool { return mayWrite(bob, g.ID) })

	require.NoError(t, alice.ChatMainChannelSave(ref, node.ChannelSpec{ReadOnly: true}))
	eventually(t, "the main channel is read-only for bob", func() bool {
		mc, err := bob.ChatMainChannel(ref)
		return err == nil && mc.ReadOnly && !mc.CanSend
	})
	post, err := alice.ChatSend(ref, node.SendRequest{Text: "news"})
	require.NoError(t, err)
	eventually(t, "bob has the post", func() bool {
		_, err := bob.ChatMessage(ref, post.ID)
		return err == nil
	})

	path := filepath.Join(t.TempDir(), "log.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"fps":48}`), 0o600))
	_, err = bob.ChatSend(ref, node.SendRequest{Text: "me too"})
	require.Error(t, err, "a plain member does not post in an announcements channel")
	_, err = bob.SendGroupFile(g.ID, "", "", path)
	require.Error(t, err, "nor sends a file there")
	_, err = bob.SendGroupFile(g.ID, "", post.ID, path)
	require.NoError(t, err, "a file as a comment in a thread is allowed")
}
