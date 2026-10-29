package node

import (
	"crypto/ed25519"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// While somebody no longer a member still holds the epoch in use (the next one waits for the rekey
// gap), nothing new is sealed with it: the members get it in their own sessions, and the one who
// left gets nothing. Once the new epoch is out, messages are sealed again.
func TestNothingIsSealedWithATaintedEpoch(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 4)
	a, b, c, d := ns[0], ns[1], ns[2], ns[3]
	for _, n := range ns {
		n.say(t, g, "hi from "+n.id.String()[:4]) // everybody learns everybody's version
	}
	m.tick(time.Hour) // a quiet group: the first departure gets a new epoch at once

	require.NoError(t, a.f.Remove(g, c.id.String(), false))
	m.settle()
	require.NoError(t, a.f.Remove(g, d.id.String(), false)) // within the gap: the rekey waits
	m.settle()

	sealed, plain := m.kind("smsg"), m.kind("msg")
	b.say(t, g, "after d left")
	assert.Equal(t, sealed, m.kind("smsg"), "nothing is sealed with an epoch d still holds")
	assert.Greater(t, m.kind("msg"), plain, "the message went in the members' own sessions")
	assert.Contains(t, a.texts(t, g), "after d left")
	assert.NotContains(t, d.texts(t, g), "after d left")

	m.tick(rekeyGapMax) // the replacement is made
	sealed = m.kind("smsg")
	b.say(t, g, "sealed again")
	assert.Greater(t, m.kind("smsg"), sealed, "a clean epoch seals again")
	assert.Contains(t, a.texts(t, g), "sealed again")
}

// A ban outlives a restart: the banned peer cannot be added back.
func TestBansSurviveARestart(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, c := ns[0], ns[2]
	require.NoError(t, a.f.Remove(g, c.id.String(), true))
	m.settle()
	require.Error(t, a.f.AddMember(g, c.pubHex), "banned")

	a.start() // the owner's app restarts
	assert.Error(t, a.f.AddMember(g, c.pubHex), "a banned peer cannot be added after a restart")
}

// groupEvent is a signed log message of n in group g.
func (n *meshNode) groupEvent(t *testing.T, g string, seq uint64, text string) []byte {
	t.Helper()
	body, err := proto.Marshal(&convlog.MsgBody{Text: text})
	require.NoError(t, err)
	gc := convlog.GroupConv(g)
	e := &convlog.Event{Conv: gc[:], Author: n.pub, Seq: seq, Lamport: seq, TsMs: time.Now().UnixMilli(),
		Kind: convlog.KindMsg, Body: body}
	require.NoError(t, convlog.Sign(e, func(d []byte) ([]byte, error) { return ed25519.Sign(n.priv, d), nil }))
	raw, err := convlog.Encode(e)
	require.NoError(t, err)
	return raw
}

// collect hooks the node's group service to record the log events it receives.
func (n *meshNode) collect() (*sync.Mutex, *[][]byte) {
	var mu sync.Mutex
	var got [][]byte
	n.f.onEvent = func(_ string, _ types.PeerID, raw []byte) {
		mu.Lock()
		got = append(got, raw)
		mu.Unlock()
	}
	return &mu, &got
}

// A group runs on the log once every member speaks it; its events travel sealed like messages, and
// an event whose author is not the member that sealed it is dropped.
func TestGroupEventsTravelSealedOnceEveryoneSpeaksTheLog(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b, c := ns[0], ns[1], ns[2]
	for _, n := range ns {
		n.say(t, g, "hi") // versions travel with every group message
	}
	for _, n := range ns {
		assert.True(t, n.f.LogMode(g), "%s: every member speaks the log", n.id.String()[:6])
	}
	muB, gotB := b.collect()
	muC, gotC := c.collect()

	before := m.kind("smsg")
	ev := a.groupEvent(t, g, 1, "an event")
	a.f.postEvent(g, ev, true, "an event")
	m.settle()
	assert.Equal(t, before+2, m.kind("smsg"), "sealed to both others")
	for name, x := range map[string]struct {
		mu  *sync.Mutex
		got *[][]byte
	}{"b": {muB, gotB}, "c": {muC, gotC}} {
		x.mu.Lock()
		require.Len(t, *x.got, 1, name)
		assert.Equal(t, ev, (*x.got)[0], name)
		x.mu.Unlock()
	}

	// a member sealing an event another member wrote (or forged) is not believed
	forged := b.groupEvent(t, g, 2, "not from a")
	a.f.postEvent(g, forged, true, "")
	m.settle()
	muC.Lock()
	assert.Len(t, *gotC, 1, "an event sealed by someone other than its author is dropped")
	muC.Unlock()
}

// A big group's event goes straight to a few members only, connected ones first; a small group's
// to everybody.
func TestBigGroupsPushToAFew(t *testing.T) {
	f := &groupsFacade{}
	var members []groups.MemberProfile
	online := map[types.PeerID]bool{}
	for i := 0; i < 60; i++ {
		var id types.PeerID
		id[0], id[1] = byte(i), 1
		members = append(members, groups.MemberProfile{PeerID: id})
		if i%20 == 0 {
			online[id] = true
		}
	}
	f.connected = func(id types.PeerID) bool { return online[id] }
	picked := f.pushTargets(members)
	require.Len(t, picked, pushFanout)
	got := 0
	for _, m := range picked {
		if online[m.PeerID] {
			got++
		}
	}
	assert.Equal(t, len(online), got, "every connected member is among them")
	assert.Len(t, f.pushTargets(members[:10]), 10, "a small group: everybody")
}

// A member on an older version keeps the group on the old way, where the log's operations are not
// available.
func TestAnOlderMemberKeepsTheGroupOffTheLog(t *testing.T) {
	m := newMesh()
	a := m.add(t)
	old := m.add(t)
	old.legacy = true
	g, err := a.f.Create("Mixed", "", "group")
	require.NoError(t, err)
	joinAll(t, m, a, g.ID, old)
	a.say(t, g.ID, "hello")
	old.say(t, g.ID, "hi")
	assert.False(t, a.f.LogMode(g.ID))
}

// A ban that reaches a member only by the group's log, not by the remover's "rm" message (the
// member was away and that message was lost), still takes the banned peer off its list: it no
// longer hands them the group's keys or lets them back in. A removal that came both ways counts
// once.
func TestBanByTheLogReachesAMemberWhoMissedTheMessage(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b, c := ns[0], ns[1], ns[2]

	m.setOnline(b, false)
	require.NoError(t, a.f.Remove(g, c.id.String(), true))
	// the "rm" for b is lost on the way
	m.mu.Lock()
	keep := m.held[:0]
	for _, d := range m.held {
		if d.to != b.id {
			keep = append(keep, d)
		}
	}
	m.held = keep
	m.mu.Unlock()
	m.setOnline(b, true)
	m.settle()

	b.f.mu.Lock()
	_, still := b.f.member(g, c.id)
	b.f.mu.Unlock()
	require.True(t, still, "the test needs b to have missed the removal")

	b.f.removedOnLog(g, a.id, c.pub, true) // the ban event of the log arrives
	m.settle()
	b.f.mu.Lock()
	_, still = b.f.member(g, c.id)
	b.f.mu.Unlock()
	assert.False(t, still, "the log's ban takes c off b's list")

	b.f.removedOnLog(g, a.id, c.pub, true) // again, as when the "rm" also comes: nothing changes
	m.settle()

	// a member cannot use the log path to remove somebody it may not
	b.f.removedOnLog(g, c.id, a.pub, true)
	b.f.mu.Lock()
	_, ownerIn := b.f.member(g, a.id)
	b.f.mu.Unlock()
	assert.True(t, ownerIn, "a removal by somebody without the right is refused by the group service")
}

// A role the group's log defines is one the group service gives and follows: a member given it
// gets its permissions on every node, they change when the definition does, and the members of a
// role that went get the default one.
func TestCustomRolesReachTheGroupService(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b, c := ns[0], ns[1], ns[2]
	helper := convlog.Role{Name: "helper", Title: "Helper", Color: 0x2266ff, Rank: 30,
		Perms: uint32(groups.RoleMember.Permissions | groups.PermPin)}
	for _, n := range ns {
		n.f.mgr.SetRoleResolver(n.f.resolveRole)
		n.f.takeRoles(g, []convlog.Role{helper})
	}
	require.NoError(t, a.f.SetRole(g, c.id.String(), "helper"))
	m.settle()
	roleOn := func(n *meshNode) groups.Role {
		r, ok := n.f.role(g, c.id)
		require.True(t, ok)
		return r
	}
	for _, n := range []*meshNode{a, b, c} {
		r := roleOn(n)
		assert.Equal(t, "helper", r.Name)
		assert.Equal(t, "Helper", r.Title)
		assert.True(t, groups.HasPermission(r, groups.PermPin), "a helper pins")
	}

	helper.Perms = uint32(groups.RoleMember.Permissions)
	b.f.takeRoles(g, []convlog.Role{helper}) // the narrowed definition reaches b's log
	assert.False(t, groups.HasPermission(roleOn(b), groups.PermPin), "the narrowed role does not pin")

	b.f.takeRoles(g, nil) // the role went
	r := roleOn(b)
	assert.Equal(t, groups.RoleMember.Name, r.Name)
	assert.Equal(t, groups.RoleMember.Permissions, r.Permissions)

	// a member list naming a role b does not know yet keeps the name, with a member's rights
	b.f.mu.Lock()
	err := b.f.mgr.MergeMembers(g, memberProfiles([]wireMember{{ID: c.id.String(), Pub: hex.EncodeToString(c.pub[:]), Role: "helper"}}), true)
	b.f.mu.Unlock()
	require.NoError(t, err)
	r = roleOn(b)
	assert.Equal(t, "helper", r.Name)
	assert.Equal(t, groups.RoleMember.Permissions, r.Permissions)
}
