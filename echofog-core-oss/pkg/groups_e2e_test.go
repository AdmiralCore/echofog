package pkg

import (
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/node"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func addrOf(n *coreNode.Node) string {
	return network.JoinPeerAddr(n.Transport.Addr().String(), n.ID)
}

func groupTexts(a *node.NodeApi, group string) []string {
	list, _ := a.GetGroupMessages(group, 0)
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.Text)
	}
	return out
}

// Three real nodes: a group made by Alice, joined by Bob and Carol with invites. Messages reach
// everybody over the real encrypted transport, and a call started in the group rings the others.
func TestThreeNodesShareAGroup(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob")
	carolN, carol := startNode(t, "carol")

	var bobEvents, carolEvents []node.GroupEvent
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	record := func(dst *[]node.GroupEvent) func(node.GroupEvent) {
		return func(ev node.GroupEvent) { <-mu; *dst = append(*dst, ev); mu <- struct{}{} }
	}
	bob.SubscribeGroupEvents(record(&bobEvents))
	carol.SubscribeGroupEvents(record(&carolEvents))
	events := func(src *[]node.GroupEvent, kind string) (out []node.GroupEvent) {
		<-mu
		defer func() { mu <- struct{}{} }()
		for _, e := range *src {
			if e.Kind == kind {
				out = append(out, e)
			}
		}
		return
	}

	// Alice is the hub that the others know; everybody else finds each other through the group
	require.NoError(t, bob.ConnectToPeer(addrOf(aliceN)))
	require.NoError(t, carol.ConnectToPeer(addrOf(aliceN)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) == 2 }, 10*time.Second, 100*time.Millisecond)

	g, err := alice.CreateGroup("Crew", "", "group")
	require.NoError(t, err)
	invite, err := alice.GetGroupInvite(g.ID)
	require.NoError(t, err)
	_, err = bob.JoinGroup(invite)
	require.NoError(t, err)
	_, err = carol.JoinGroup(invite)
	require.NoError(t, err)

	// everybody learns who is in the group
	require.Eventually(t, func() bool {
		for _, a := range []*node.NodeApi{alice, bob, carol} {
			m, err := a.ListGroupMembers(g.ID)
			if err != nil || len(m) != 3 {
				return false
			}
		}
		return true
	}, 15*time.Second, 100*time.Millisecond, "all three should know all three")

	// messages in every direction
	_, err = bob.SendGroupMessage(g.ID, "hello from bob")
	require.NoError(t, err)
	_, err = carol.SendGroupMessage(g.ID, "hello from carol")
	require.NoError(t, err)
	_, err = alice.SendGroupMessage(g.ID, "hello from alice")
	require.NoError(t, err)

	want := []string{"hello from bob", "hello from carol", "hello from alice"}
	require.Eventually(t, func() bool {
		for _, a := range []*node.NodeApi{alice, bob, carol} {
			if len(groupTexts(a, g.ID)) != 3 {
				return false
			}
		}
		return true
	}, 20*time.Second, 100*time.Millisecond, "every message must reach every member")
	for _, a := range []*node.NodeApi{alice, bob, carol} {
		assert.ElementsMatch(t, want, groupTexts(a, g.ID))
	}
	assert.NotEmpty(t, events(&bobEvents, "message"), "the app is told about new messages")

	// group messages are not shown as direct chat lines
	direct, _ := alice.GetMessages(bobN.ID.String(), 50, 0)
	for _, m := range direct {
		assert.False(t, strings.HasPrefix(m.Text, "fogrp2:"), "protocol traffic leaked into the chat: %q", m.Text)
	}

	// a call started in the group rings the others
	call, err := alice.StartGroupCall(g.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(events(&bobEvents, "call")) > 0 && len(events(&carolEvents, "call")) > 0
	}, 15*time.Second, 100*time.Millisecond, "the call must ring")
	ring := events(&bobEvents, "call")[0]
	assert.Equal(t, call.CallID, ring.CallID)
	assert.Equal(t, aliceN.ID.String(), ring.Coordinator)
	_ = carolN

	// an admin removes a member: the group disappears for them
	require.NoError(t, alice.RemoveGroupMember(g.ID, carolN.ID.String(), false))
	require.Eventually(t, func() bool {
		l, _ := carol.ListGroups()
		return len(l) == 0
	}, 15*time.Second, 100*time.Millisecond, "the removed member keeps the group")
}

// History for a latecomer, handing the group over, the end of a call and deleting the group, on
// real nodes.
func TestGroupLifecycleOverTheNetwork(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob")
	_, dave := startNode(t, "dave")

	var daveEvents []node.GroupEvent
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	dave.SubscribeGroupEvents(func(ev node.GroupEvent) { <-lock; daveEvents = append(daveEvents, ev); lock <- struct{}{} })
	seen := func(kind string) bool {
		<-lock
		defer func() { lock <- struct{}{} }()
		for _, e := range daveEvents {
			if e.Kind == kind {
				return true
			}
		}
		return false
	}

	require.NoError(t, bob.ConnectToPeer(addrOf(aliceN)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) == 1 }, 10*time.Second, 100*time.Millisecond)

	g, err := alice.CreateGroup("Crew", "", "group")
	require.NoError(t, err)
	invite, _ := alice.GetGroupInvite(g.ID)
	_, err = bob.JoinGroup(invite)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		m, _ := alice.ListGroupMembers(g.ID)
		return len(m) == 2
	}, 10*time.Second, 100*time.Millisecond)

	_, err = alice.SendGroupMessage(g.ID, "before dave")
	require.NoError(t, err)
	_, err = bob.SendGroupMessage(g.ID, "also before dave")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(groupTexts(alice, g.ID)) == 2 && len(groupTexts(bob, g.ID)) == 2 }, 15*time.Second, 100*time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	// a latecomer reads what was said before
	require.NoError(t, dave.ConnectToPeer(addrOf(aliceN)))
	inviteB, _ := bob.GetGroupInvite(g.ID)
	_, err = dave.JoinGroup(inviteB)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(groupTexts(dave, g.ID)) == 2 }, 20*time.Second, 100*time.Millisecond, "the earlier messages must arrive")
	assert.ElementsMatch(t, []string{"before dave", "also before dave"}, groupTexts(dave, g.ID))
	assert.Eventually(t, func() bool { return seen("history") }, 5*time.Second, 50*time.Millisecond)

	// everybody knows everybody before the next steps
	require.Eventually(t, func() bool {
		for _, a := range []*node.NodeApi{alice, bob, dave} {
			if m, _ := a.ListGroupMembers(g.ID); len(m) != 3 {
				return false
			}
		}
		return true
	}, 15*time.Second, 100*time.Millisecond)

	// a call: it rings, then it ends, and nobody is offered it any more
	call, err := alice.StartGroupCall(g.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, ok := dave.ActiveGroupCall(g.ID); return ok }, 15*time.Second, 100*time.Millisecond)
	require.NoError(t, alice.EndGroupCall(g.ID, call.CallID))
	require.Eventually(t, func() bool {
		_, okD := dave.ActiveGroupCall(g.ID)
		_, okB := bob.ActiveGroupCall(g.ID)
		return !okD && !okB && seen("callend")
	}, 15*time.Second, 100*time.Millisecond, "the end of the call must reach the others")

	// ownership: Bob becomes the owner, Alice an admin
	require.NoError(t, alice.TransferGroupOwnership(g.ID, bobN.ID.String()))
	roleOf := func(a *node.NodeApi, who string) string {
		ms, _ := a.ListGroupMembers(g.ID)
		for _, m := range ms {
			if m.PeerID == who {
				return m.Role
			}
		}
		return ""
	}
	require.Eventually(t, func() bool {
		return roleOf(dave, bobN.ID.String()) == "owner" && roleOf(dave, aliceN.ID.String()) == "admin" && roleOf(bob, bobN.ID.String()) == "owner"
	}, 15*time.Second, 100*time.Millisecond, "the new owner must be known to everybody")
	assert.Error(t, alice.DeleteGroup(g.ID), "the former owner cannot delete the group")

	// and the new owner deletes it for everybody
	require.NoError(t, bob.DeleteGroup(g.ID))
	require.Eventually(t, func() bool {
		a, _ := alice.ListGroups()
		d, _ := dave.ListGroups()
		return len(a) == 0 && len(d) == 0
	}, 15*time.Second, 100*time.Millisecond, "the group must disappear for everybody")
}
