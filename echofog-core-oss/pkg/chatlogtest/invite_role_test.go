package chatlogtest

import (
	"testing"

	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/stretchr/testify/require"
)

// An invitation that gives a role lets its newcomer in with that role, on the log too: Bob,
// joining a group on the log with Alice's admin invitation, is an admin for both of them and may
// do what an admin does.
func TestRoleInvitationGivesItsRole(t *testing.T) {
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
	inv, err := alice.GroupInviteCreate(g.ID, node.GroupInviteSpec{Label: "co-admin", Hours: 24, Uses: 1, Role: "admin"})
	require.NoError(t, err)
	require.NotEmpty(t, inv.Link)
	_, err = bob.JoinGroup(inv.Link)
	require.NoError(t, err)

	bobID := bobN.ID.String()
	roleAt := func(a *node.NodeApi) string {
		list, err := a.ListGroupMembers(g.ID)
		if err != nil {
			return ""
		}
		for _, m := range list {
			if m.PeerID == bobID {
				return m.Role
			}
		}
		return ""
	}
	mayManageRoles := func(a *node.NodeApi) bool {
		rows, err := a.ChatRows()
		if err != nil {
			return false
		}
		for _, r := range rows {
			if r.Ref.ID == g.ID {
				return r.Can.Roles
			}
		}
		return false
	}
	eventually(t, "alice has bob as an admin", func() bool { return roleAt(alice) == "admin" })
	eventually(t, "bob is an admin on his side, on the log", func() bool { return roleAt(bob) == "admin" && mayManageRoles(bob) })
	_, err = bob.ChatChannelSave(groupRef(g.ID), node.ChannelSpec{Name: "ops"})
	require.NoError(t, err, "an admin sets up channels")
}
