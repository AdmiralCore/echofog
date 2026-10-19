package filetransfer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"
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
	posted, err := alice.SendGroupFile(g.ID, src)
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
