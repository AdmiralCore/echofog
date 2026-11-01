package chatlogtest

import (
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/stretchr/testify/require"
)

// A group file (.fogzip) carries a group to a newcomer who cannot reach any member: Bob, never
// connected to Alice, imports Alice's file and reads the group's messages, channels and thread
// names; the wrong password opens nothing, and the group waits for a member to let him in.
func TestGroupFileCarriesTheGroup(t *testing.T) {
	_, alice := startNode(t, "alice")
	_, bob := startNode(t, "bob")
	for _, a := range []*node.NodeApi{alice, bob} {
		_, err := a.Conversations()
		require.NoError(t, err)
	}
	g, err := alice.CreateGroup("Crew", "", "group")
	require.NoError(t, err)
	ref := groupRef(g.ID)
	eventually(t, "the group runs on the log", func() bool { return upgraded(t, alice, g.ID) })
	dev, err := alice.ChatChannelSave(ref, node.ChannelSpec{Name: "dev"})
	require.NoError(t, err)
	root, err := alice.ChatSend(ref, node.SendRequest{Text: "the plan"})
	require.NoError(t, err)
	_, err = alice.ChatSend(ref, node.SendRequest{Text: "a comment", Thread: root.ID})
	require.NoError(t, err)
	require.NoError(t, alice.ChatThreadTitle(ref, root.ID, "Plan talk"))

	path := filepath.Join(t.TempDir(), "crew.fogzip")
	require.Error(t, alice.GroupSnapshotExport(g.ID, path, "short"), "a weak password")
	require.NoError(t, alice.GroupSnapshotExport(g.ID, path, "correct horse"))

	_, err = bob.GroupSnapshotImport(path, "wrong password")
	require.Error(t, err)
	info, err := bob.GroupSnapshotImport(path, "correct horse")
	require.NoError(t, err)
	require.Equal(t, g.ID, info.Group.ID)
	require.Greater(t, info.Events, 0)

	page, err := bob.ChatHistory(ref, "", 20)
	require.NoError(t, err)
	var found bool
	for _, m := range page.Messages {
		if m.ID == root.ID {
			found = true
			require.Equal(t, "Plan talk", m.ThreadTitle)
			require.Equal(t, 1, m.Replies)
		}
	}
	require.True(t, found, "the history came with the file")
	chans, err := bob.ChatChannels(ref)
	require.NoError(t, err)
	require.Len(t, chans, 1)
	require.Equal(t, dev.ID, chans[0].ID)

	rows, err := bob.ChatRows()
	require.NoError(t, err)
	for _, r := range rows {
		if r.Ref.ID == g.ID {
			require.NotEmpty(t, r.Pending, "nobody let bob in yet")
		}
	}
}
