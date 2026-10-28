package filetransfer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// groupFileMessage waits for the group message that carries file id.
func groupFileMessage(t *testing.T, api *node.NodeApi, group, id string) node.GroupMessageDTO {
	t.Helper()
	var found node.GroupMessageDTO
	require.Eventually(t, func() bool {
		msgs, _ := api.GetGroupMessages(group, 50)
		for _, m := range msgs {
			if m.File != nil && m.File.ID == id {
				found = m
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "the file message never arrived")
	return found
}

// One file posted in a group of three: each of the other two accepts and pulls it from the
// sender, and the sender's card counts who has it.
func TestGroupFileToTwoMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 2 x 24 MB")
	}
	aN, alice := fileNode(t, "gfiles-alice", nil)
	_, bob := fileNode(t, "gfiles-bob", nil)
	_, carol := fileNode(t, "gfiles-carol", nil)
	join := network.JoinPeerAddr(aN.Transport.Addr().String(), aN.ID)
	require.NoError(t, bob.ConnectToPeer(join))
	require.NoError(t, carol.ConnectToPeer(join))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) >= 2 }, 10*time.Second, 50*time.Millisecond)

	g, err := alice.CreateGroup("Files", "", "group")
	require.NoError(t, err)
	invite, err := alice.GetGroupInvite(g.ID)
	require.NoError(t, err)
	_, err = bob.JoinGroup(invite)
	require.NoError(t, err)
	_, err = carol.JoinGroup(invite)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		for _, a := range []*node.NodeApi{alice, bob, carol} {
			if m, err := a.ListGroupMembers(g.ID); err != nil || len(m) != 3 {
				return false
			}
		}
		return true
	}, 15*time.Second, 100*time.Millisecond, "all three should know all three")

	const size = 24<<20 + 4321
	src, sum := bigFile(t, t.TempDir(), "report.pdf", size)
	posted, err := alice.SendGroupFile(g.ID, "", "", src)
	require.NoError(t, err)
	require.NotNil(t, posted.File)
	assert.Equal(t, "report.pdf", posted.Text)
	assert.Equal(t, int64(size), posted.File.Size)
	id := posted.File.ID

	out, err := alice.GetFileTransfer(id)
	require.NoError(t, err)
	assert.Equal(t, g.ID, out.Group)
	assert.Equal(t, 2, out.Members, "the sender does not count itself")

	for _, r := range []*node.NodeApi{bob, carol} {
		m := groupFileMessage(t, r, g.ID, id)
		raw, _ := json.Marshal(m)
		assert.NotContains(t, string(raw), "\"k\"", "the file key is not kept in the message")
		in := waitTransfer(t, r, id, "offered", 5*time.Second)
		assert.Equal(t, g.ID, in.Group)
		assert.Equal(t, "report.pdf", in.Name)
		_, err := r.AcceptFile(id)
		require.NoError(t, err)
	}
	for _, r := range []*node.NodeApi{bob, carol} {
		done := waitTransfer(t, r, id, "done", 2*time.Minute)
		assert.Equal(t, sum, fileSum(t, done.Path), "the received file differs from the sent one")
	}
	require.Eventually(t, func() bool {
		o, _ := alice.GetFileTransfer(id)
		return o.Delivered == 2
	}, 15*time.Second, 50*time.Millisecond, "the sender learns that both have it")
	o, _ := alice.GetFileTransfer(id)
	assert.Equal(t, "active", o.State, "a group file stays offered")
}

// hasFile reports whether page holds the message offering file id.
func hasFile(page node.ChatPage, id string) bool {
	for _, m := range page.Messages {
		if m.File != nil && m.File.ID == id {
			return true
		}
	}
	return false
}

// A file posted in a channel of a space, or in a thread, shows there for the other members, not
// in the main channel.
func TestGroupFileInChannelAndThread(t *testing.T) {
	aN, alice := fileNode(t, "gchan-alice", nil)
	_, bob := fileNode(t, "gchan-bob", nil)
	require.NoError(t, bob.ConnectToPeer(network.JoinPeerAddr(aN.Transport.Addr().String(), aN.ID)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) >= 1 }, 10*time.Second, 50*time.Millisecond)

	g, err := alice.CreateGroup("Space", "", "group")
	require.NoError(t, err)
	invite, err := alice.GetGroupInvite(g.ID)
	require.NoError(t, err)
	_, err = bob.JoinGroup(invite)
	require.NoError(t, err)
	ref := node.ChatRef{Kind: convlog.KindGroup, ID: g.ID}
	var ch node.ChannelDTO
	require.Eventually(t, func() bool {
		ch, err = alice.ChatChannelSave(ref, node.ChannelSpec{Name: "photos"})
		return err == nil
	}, 15*time.Second, 100*time.Millisecond, "the space should take a channel")
	in := node.ChatRef{Kind: convlog.KindGroup, ID: g.ID, Channel: ch.ID}
	require.Eventually(t, func() bool {
		list, _ := bob.ChatChannels(ref)
		return len(list) > 0
	}, 15*time.Second, 100*time.Millisecond, "bob learns of the channel")

	src, _ := bigFile(t, t.TempDir(), "cat.jpg", 4096)
	posted, err := alice.SendGroupFile(g.ID, ch.ID, "", src)
	require.NoError(t, err)
	id := posted.File.ID
	for _, r := range []*node.NodeApi{alice, bob} {
		require.Eventually(t, func() bool {
			page, _ := r.ChatHistory(in, "", 50)
			return hasFile(page, id)
		}, 15*time.Second, 50*time.Millisecond, "the file shows in its channel")
		main, _ := r.ChatHistory(ref, "", 50)
		assert.False(t, hasFile(main, id), "the file is not in the main channel")
	}

	root, err := alice.ChatSend(in, node.SendRequest{Text: "comments here"})
	require.NoError(t, err)
	src2, _ := bigFile(t, t.TempDir(), "dog.jpg", 4096)
	posted, err = alice.SendGroupFile(g.ID, ch.ID, root.ID, src2)
	require.NoError(t, err)
	id = posted.File.ID
	require.Eventually(t, func() bool {
		page, _ := bob.ChatThread(in, root.ID, "", 50)
		return hasFile(page, id)
	}, 15*time.Second, 50*time.Millisecond, "the file shows in the thread")
	page, _ := bob.ChatHistory(in, "", 50)
	assert.False(t, hasFile(page, id), "a comment is not in the channel's own history")
}
