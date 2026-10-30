package chatlogtest

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/stretchr/testify/require"
)

// A group's picture, set by its owner, is the one every member shows; a plain member cannot set
// it, a file that is not a small image is refused, and taking it away reaches everybody too.
func TestGroupPhotoReachesEveryMember(t *testing.T) {
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

	photoAt := func(a *node.NodeApi) string {
		rows, err := a.ChatRows()
		if err != nil {
			return "?"
		}
		for _, r := range rows {
			if r.Ref.ID == g.ID {
				return r.Avatar
			}
		}
		return "?"
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte(strings.Repeat("x", 200))...)
	pic := base64.StdEncoding.EncodeToString(png)
	require.Error(t, alice.ChatGroupPhoto(ref, base64.StdEncoding.EncodeToString([]byte("not an image"))))
	require.Error(t, alice.ChatGroupPhoto(ref, base64.StdEncoding.EncodeToString(append(png, make([]byte, 64<<10)...))), "too large")
	require.NoError(t, alice.ChatGroupPhoto(ref, pic))
	require.Equal(t, pic, photoAt(alice))
	eventually(t, "bob shows alice's picture", func() bool { return photoAt(bob) == pic })
	eventually(t, "bob is in on the log", func() bool {
		rows, _ := bob.ChatRows()
		for _, r := range rows {
			if r.Ref.ID == g.ID {
				return r.Pending == "" && r.Can.Send
			}
		}
		return false
	})
	require.Error(t, bob.ChatGroupPhoto(ref, ""), "a plain member does not change it")

	require.NoError(t, alice.ChatGroupPhoto(ref, ""))
	eventually(t, "taken away for bob too", func() bool { return photoAt(bob) == "" })
}

// A posted invitation is read for its card without joining: the group's title and how many it
// names, and whether I am in it already.
func TestGroupInviteReadsWithoutJoining(t *testing.T) {
	_, alice := startNode(t, "alice")
	_, bob := startNode(t, "bob")
	g, err := alice.CreateGroup("Crew", "", "group")
	require.NoError(t, err)
	invite, err := alice.GetGroupInvite(g.ID)
	require.NoError(t, err)
	web := node.GroupWebLink(invite)
	require.NotEmpty(t, web)
	for _, text := range []string{invite, "come in: " + web + " ."} {
		p, err := bob.InspectGroupInvite(text)
		require.NoError(t, err)
		require.Equal(t, "Crew", p.Title)
		require.Equal(t, g.ID, p.GroupID)
		require.Equal(t, "group", p.Kind)
		require.False(t, p.Joined)
		require.False(t, p.Expired)
	}
	groups, err := bob.ListGroups()
	require.NoError(t, err)
	require.Empty(t, groups, "reading it joined nothing")
	p, err := alice.InspectGroupInvite(invite)
	require.NoError(t, err)
	require.True(t, p.Joined)
	_, err = bob.InspectGroupInvite("fogroup2:bm90IGpzb24")
	require.Error(t, err)
}

// Contacts invited to a group get an invitation of their own in their direct chat, as a message
// their app turns into a card, and it lets them in.
func TestContactsAreInvitedInTheirChats(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob")
	for _, a := range []*node.NodeApi{alice, bob} {
		_, err := a.Conversations()
		require.NoError(t, err)
	}
	require.NoError(t, bob.ConnectToPeer(joinOf(aliceN)))
	eventually(t, "connected", func() bool { return len(alice.GetPeers()) == 1 })
	g, err := alice.CreateGroup("Crew", "", "group")
	require.NoError(t, err)
	eventually(t, "the group runs on the log", func() bool { return upgraded(t, alice, g.ID) })

	sent, err := alice.GroupInviteContacts(g.ID, []string{bobN.ID.String()}, "")
	require.NoError(t, err)
	require.Equal(t, 1, sent)
	dm := dmRef(aliceN.ID.String())
	var text string
	eventually(t, "bob has the invitation in his chat with alice", func() bool {
		page, err := bob.ChatHistory(dm, "", 10)
		if err != nil || len(page.Messages) == 0 {
			return false
		}
		text = page.Messages[len(page.Messages)-1].Text
		return true
	})
	p, err := bob.InspectGroupInvite(text)
	require.NoError(t, err)
	require.Equal(t, "Crew", p.Title)
	_, err = bob.JoinGroup(text)
	require.NoError(t, err)
	eventually(t, "bob is in", func() bool {
		list, err := alice.ListGroupMembers(g.ID)
		return err == nil && len(list) == 2
	})
	invs, err := alice.GroupInvites(g.ID)
	require.NoError(t, err)
	require.Len(t, invs, 1)
	require.Equal(t, uint32(1), invs[0].Uses, "one person's invitation")
}
