package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hub stands in for the network: it hands a message of one node to the group service of another,
// the way the messenger does after decrypting it, or keeps it while that node is offline.
type hub struct {
	mu      sync.Mutex
	nodes   map[types.PeerID]*testNode
	offline map[types.PeerID]bool
	waiting map[types.PeerID][]queuedMsg
}

type queuedMsg struct {
	from types.PeerID
	text string
}

type testNode struct {
	f      *groupsFacade
	id     types.PeerID
	pub    ed25519.PublicKey
	pubHex string
	h      *hub
	mu     sync.Mutex
	events []GroupEvent
	known  map[types.PeerID]bool // contacts
}

func newHub() *hub {
	return &hub{nodes: map[types.PeerID]*testNode{}, offline: map[types.PeerID]bool{}, waiting: map[types.PeerID][]queuedMsg{}}
}

func (h *hub) node(t testing.TB, store storage.Storage) *testNode {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var ident types.IdentityPublicKey
	copy(ident[:], pub)
	id := types.PeerID(sha256.Sum256(pub))
	n := &testNode{id: id, pub: pub, pubHex: hex.EncodeToString(pub), h: h, known: map[types.PeerID]bool{}}
	n.f = newGroupsFacade(store, id, ident, func(d []byte) ([]byte, error) { return ed25519.Sign(priv, d), nil },
		func(target types.PeerID, text string) (bool, error) { return h.send(id, target, text) },
		func(p types.PeerID) bool { n.mu.Lock(); defer n.mu.Unlock(); return n.known[p] },
		func(ev GroupEvent) { n.mu.Lock(); n.events = append(n.events, ev); n.mu.Unlock() }, nil)
	h.mu.Lock()
	h.nodes[id] = n
	h.mu.Unlock()
	return n
}

func (h *hub) send(from, to types.PeerID, text string) (bool, error) {
	h.mu.Lock()
	target := h.nodes[to]
	if target == nil {
		h.mu.Unlock()
		return false, assert.AnError
	}
	if h.offline[to] {
		h.waiting[to] = append(h.waiting[to], queuedMsg{from, text})
		h.mu.Unlock()
		return true, nil
	}
	h.mu.Unlock()
	target.f.handle(from, text)
	return false, nil
}

// online brings a node back and delivers what waited for it.
func (h *hub) online(n *testNode) {
	h.mu.Lock()
	delete(h.offline, n.id)
	queued := h.waiting[n.id]
	delete(h.waiting, n.id)
	h.mu.Unlock()
	for _, m := range queued {
		n.f.handle(m.from, m.text)
	}
}

func (n *testNode) eventKinds() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.events))
	for _, e := range n.events {
		out = append(out, e.Kind)
	}
	return out
}

func texts(t *testing.T, n *testNode, group string) []string {
	t.Helper()
	list, err := n.f.Messages(group, 0)
	require.NoError(t, err)
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.Text)
	}
	return out
}

func roleOf(t *testing.T, n *testNode, group string, who *testNode) string {
	t.Helper()
	list, err := n.f.Members(group)
	require.NoError(t, err)
	for _, m := range list {
		if m.PeerID == who.id.String() {
			return m.Role
		}
	}
	return ""
}

func TestGroupsFacade_CreateListPersist(t *testing.T) {
	store := newMemStore()
	n := newHub().node(t, store)

	g, err := n.f.Create("Devs", "build things", "group")
	require.NoError(t, err)
	assert.Equal(t, "owner", g.MyRole)
	assert.False(t, g.ReadOnly)
	assert.Equal(t, 1, g.Members)

	_, err = n.f.Create("  ", "", "group")
	assert.Error(t, err)
	_, err = n.f.Create("x", "", "bogus")
	assert.Error(t, err)

	reborn := newGroupsFacade(store, n.f.selfID, n.f.selfPub, n.f.sign, nil, nil, nil, nil)
	list, err := reborn.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, g.ID, list[0].ID)
	assert.Equal(t, "owner", list[0].MyRole)
}

func TestGroupsFacade_InviteJoinVerifiesSignature(t *testing.T) {
	h := newHub()
	owner, guest := h.node(t, newMemStore()), h.node(t, newMemStore())

	g, err := owner.f.Create("Swarm", "", "channel")
	require.NoError(t, err)
	invite, err := owner.f.Invite(g.ID)
	require.NoError(t, err)

	joined, err := guest.f.Join(invite)
	require.NoError(t, err)
	assert.Equal(t, g.ID, joined.ID)
	assert.Equal(t, "subscriber", joined.MyRole)
	assert.True(t, joined.ReadOnly)

	_, err = guest.f.SendMessage(g.ID, "hello")
	assert.Error(t, err, "a channel subscriber cannot post")

	_, err = guest.f.Join("fogroup2:not-base64!")
	assert.Error(t, err)
	_, err = guest.f.Join("garbage")
	assert.Error(t, err)

	forged := invite[:len(invite)-8] + "AAAAAAAA"
	_, err = h.node(t, newMemStore()).f.Join(forged)
	assert.Error(t, err)
}

// A joins, B joins with A's invite, C with B's: everybody ends up knowing everybody, and
// messages reach all of them.
func TestGroupsFacade_MembersFindEachOtherAndTalk(t *testing.T) {
	h := newHub()
	a, b, c := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())

	g, err := a.f.Create("Crew", "", "group")
	require.NoError(t, err)
	inviteA, _ := a.f.Invite(g.ID)
	_, err = b.f.Join(inviteA)
	require.NoError(t, err)

	inviteB, err := b.f.Invite(g.ID)
	require.NoError(t, err)
	_, err = c.f.Join(inviteB)
	require.NoError(t, err)

	for _, n := range []*testNode{a, b, c} {
		list, err := n.f.Members(g.ID)
		require.NoError(t, err)
		assert.Len(t, list, 3, "every member must know all three")
	}
	assert.Equal(t, "owner", roleOf(t, c, g.ID, a), "the newcomer learns who the owner is")

	m, err := c.f.SendMessage(g.ID, "hi from C")
	require.NoError(t, err)
	assert.Equal(t, 2, m.Delivered)
	_, err = a.f.SendMessage(g.ID, "hi from A")
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"hi from C", "hi from A"}, texts(t, b, g.ID))
	assert.ElementsMatch(t, []string{"hi from C", "hi from A"}, texts(t, a, g.ID))
	assert.Contains(t, b.eventKinds(), "message")

	received, _ := b.f.Messages(g.ID, 0)
	require.NotEmpty(t, received)
	assert.Equal(t, c.id.String(), received[0].Sender)
	assert.False(t, received[0].IsMine)
}

func TestGroupsFacade_OfflineMemberGetsMessagesLater(t *testing.T) {
	h := newHub()
	a, b := h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, err := b.f.Join(invite)
	require.NoError(t, err)

	h.offline[b.id] = true
	m, err := a.f.SendMessage(g.ID, "are you there")
	require.NoError(t, err)
	assert.Equal(t, 1, m.Queued)
	assert.Empty(t, texts(t, b, g.ID))

	h.online(b)
	assert.Equal(t, []string{"are you there"}, texts(t, b, g.ID))

	// the same message again (a lost receipt makes the sender retry) is not shown twice
	b.f.handle(a.id, encodeWire(groupWire{Kind: "msg", Group: g.ID, ID: "dup", Text: "once", TS: 1}))
	b.f.handle(a.id, encodeWire(groupWire{Kind: "msg", Group: g.ID, ID: "dup", Text: "once", TS: 1}))
	assert.Equal(t, 1, countOf(texts(t, b, g.ID), "once"))
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

func TestGroupsFacade_AdminAddsMemberWhoKnowsHim(t *testing.T) {
	h := newHub()
	a, b, c := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, err := b.f.Join(invite)
	require.NoError(t, err)

	// c does not know a: the group does not simply appear
	require.NoError(t, a.f.AddMember(g.ID, c.pubHex))
	list, _ := c.f.List()
	assert.Empty(t, list, "a stranger must not be able to put us into a group")

	// once c has a as a contact it works
	c.known[a.id] = true
	require.NoError(t, a.f.AddMember(g.ID, c.pubHex))
	list, err = c.f.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "member", list[0].MyRole)
	assert.Contains(t, c.eventKinds(), "added")

	// b learned about c from a, and everyone can talk
	members, _ := b.f.Members(g.ID)
	assert.Len(t, members, 3)
	_, err = c.f.SendMessage(g.ID, "thanks for adding me")
	require.NoError(t, err)
	assert.Contains(t, texts(t, b, g.ID), "thanks for adding me")

	// a member cannot add anybody
	assert.Error(t, b.f.AddMember(g.ID, h.node(t, newMemStore()).pubHex))
}

func TestGroupsFacade_RolesKickAndBan(t *testing.T) {
	h := newHub()
	owner, admin, mod, plain := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := owner.f.Create("Crew", "", "group")
	invite, _ := owner.f.Invite(g.ID)
	for _, n := range []*testNode{admin, mod, plain} {
		_, err := n.f.Join(invite)
		require.NoError(t, err)
	}

	require.NoError(t, owner.f.SetRole(g.ID, admin.id.String(), "admin"))
	assert.Equal(t, "admin", roleOf(t, plain, g.ID, admin), "everybody sees the new role")
	require.NoError(t, admin.f.SetRole(g.ID, mod.id.String(), "moderator"))

	// ranks hold
	assert.Error(t, mod.f.SetRole(g.ID, plain.id.String(), "moderator"), "a moderator cannot manage roles")
	assert.Error(t, admin.f.SetRole(g.ID, owner.id.String(), "member"), "nobody demotes the owner")
	assert.Error(t, admin.f.SetRole(g.ID, mod.id.String(), "admin"), "an admin cannot make another admin")
	assert.Error(t, mod.f.Remove(g.ID, admin.id.String(), false), "a moderator cannot kick an admin")
	assert.Error(t, plain.f.Remove(g.ID, mod.id.String(), false))

	// a moderator may kick a plain member; the member loses the group
	require.NoError(t, mod.f.Remove(g.ID, plain.id.String(), false))
	list, _ := plain.f.List()
	assert.Empty(t, list, "the removed member no longer has the group")
	assert.Contains(t, plain.eventKinds(), "removed")
	members, _ := owner.f.Members(g.ID)
	assert.Len(t, members, 3)

	// a forged message from a non-member is ignored
	plain.f.handle(plain.id, encodeWire(groupWire{Kind: "msg", Group: g.ID, ID: "x", Text: "I am still here", TS: 1}))
	owner.f.handle(plain.id, encodeWire(groupWire{Kind: "msg", Group: g.ID, ID: "y", Text: "let me in", TS: 1}))
	assert.NotContains(t, texts(t, owner, g.ID), "let me in")

	// a plain member cannot rewrite roles by sending the message himself
	owner.f.handle(mod.id, encodeWire(groupWire{Kind: "role", Group: g.ID, Target: admin.id.String(), Role: "member"}))
	assert.Equal(t, "admin", roleOf(t, owner, g.ID, admin))

	// a ban keeps a person out even with the invite in hand
	require.NoError(t, admin.f.Remove(g.ID, mod.id.String(), true))
	_, err := mod.f.List()
	require.NoError(t, err)
	owner.f.handle(mod.id, encodeWire(groupWire{Kind: "join", Group: g.ID, Pub: mod.pubHex}))
	assert.Equal(t, "", roleOf(t, owner, g.ID, mod), "a banned member cannot join again")
}

func TestGroupsFacade_LeaveAndOwnerCannot(t *testing.T) {
	h := newHub()
	a, b := h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, err := b.f.Join(invite)
	require.NoError(t, err)

	assert.Error(t, a.f.Leave(g.ID), "the owner cannot leave")
	require.NoError(t, b.f.Leave(g.ID))
	list, _ := b.f.List()
	assert.Empty(t, list)
	members, _ := a.f.Members(g.ID)
	assert.Len(t, members, 1)
}

func TestGroupsFacade_CallRingsEveryMember(t *testing.T) {
	h := newHub()
	a, b, c := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)
	inviteB, _ := b.f.Invite(g.ID)
	_, _ = c.f.Join(inviteB)

	call, err := a.f.StartCall(g.ID, "")
	require.NoError(t, err)
	assert.Equal(t, a.id.String(), call.Coordinator)

	for _, n := range []*testNode{b, c} {
		n.mu.Lock()
		var got *GroupEvent
		for i := range n.events {
			if n.events[i].Kind == "call" {
				got = &n.events[i]
			}
		}
		n.mu.Unlock()
		require.NotNil(t, got, "every member must be told about the call")
		assert.Equal(t, call.CallID, got.CallID)
		assert.Equal(t, a.id.String(), got.Coordinator)
		assert.Equal(t, "Crew", got.Title)

		active, ok := n.f.ActiveCall(g.ID, "")
		require.True(t, ok)
		assert.Equal(t, call.CallID, active.CallID)
	}

	// somebody outside the group cannot ring it
	outsider := h.node(t, newMemStore())
	b.f.handle(outsider.id, encodeWire(groupWire{Kind: "call", Group: g.ID, CallID: "x", Coord: "y"}))
	active, _ := b.f.ActiveCall(g.ID, "")
	assert.Equal(t, call.CallID, active.CallID)
}

func TestGroupsFacade_ChannelOnlyAdminsPost(t *testing.T) {
	h := newHub()
	owner, reader := h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := owner.f.Create("News", "", "channel")
	invite, _ := owner.f.Invite(g.ID)
	_, err := reader.f.Join(invite)
	require.NoError(t, err)

	_, err = owner.f.SendMessage(g.ID, "release is out")
	require.NoError(t, err)
	assert.Equal(t, []string{"release is out"}, texts(t, reader, g.ID))

	// a subscriber who sends anyway is not shown
	owner.f.handle(reader.id, encodeWire(groupWire{Kind: "msg", Group: g.ID, ID: "spam", Text: "buy now", TS: 1}))
	assert.NotContains(t, texts(t, owner, g.ID), "buy now")
}

func TestGroupsFacade_IgnoresNonGroupText(t *testing.T) {
	n := newHub().node(t, newMemStore())
	assert.False(t, n.f.handle(n.id, "hello"))
	assert.True(t, n.f.handle(n.id, wirePrefix+"{not json"), "a broken group message is swallowed, not shown as chat")
}

func TestGroupsFacade_OwnershipMovesToAnotherMember(t *testing.T) {
	h := newHub()
	a, b, c := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)
	_, _ = c.f.Join(invite)

	assert.Error(t, b.f.TransferOwnership(g.ID, c.id.String()), "only the owner can hand the group over")
	require.NoError(t, a.f.TransferOwnership(g.ID, b.id.String()))

	for _, n := range []*testNode{a, b, c} {
		assert.Equal(t, "owner", roleOf(t, n, g.ID, b), "everybody sees the new owner")
		assert.Equal(t, "admin", roleOf(t, n, g.ID, a), "the old owner becomes an admin")
	}
	list, _ := a.f.List()
	assert.Equal(t, "admin", list[0].MyRole)

	// the new owner can do what an owner does; the old one cannot any more
	require.NoError(t, b.f.SetRole(g.ID, c.id.String(), "admin"))
	assert.Error(t, a.f.TransferOwnership(g.ID, c.id.String()))
	assert.Error(t, a.f.Delete(g.ID))
	require.NoError(t, a.f.Leave(g.ID), "the former owner may leave now")

	// somebody who joins later through an old invite still learns who owns the group
	late := h.node(t, newMemStore())
	inviteB, _ := b.f.Invite(g.ID)
	_, err := late.f.Join(inviteB)
	require.NoError(t, err)
	assert.Equal(t, "owner", roleOf(t, late, g.ID, b))
}

func TestGroupsFacade_OwnerChangeReachesAMemberWhoWasOffline(t *testing.T) {
	h := newHub()
	a, b, c := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)
	_, _ = c.f.Join(invite)

	h.offline[c.id] = true
	require.NoError(t, a.f.TransferOwnership(g.ID, b.id.String()))
	require.NoError(t, b.f.SetRole(g.ID, a.id.String(), "member")) // a second change, in order
	h.online(c)
	assert.Equal(t, "owner", roleOf(t, c, g.ID, b))
	assert.Equal(t, "member", roleOf(t, c, g.ID, a))
}

func TestGroupsFacade_OnlyTheOwnerDeletesTheGroupForEveryone(t *testing.T) {
	h := newHub()
	a, b, c := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)
	_, _ = c.f.Join(invite)
	_, _ = b.f.SendMessage(g.ID, "keep this")

	require.NoError(t, a.f.SetRole(g.ID, b.id.String(), "admin"))
	assert.Error(t, b.f.Delete(g.ID), "an admin cannot delete the group")
	// a forged delete from a non-owner changes nothing
	c.f.handle(b.id, encodeWire(groupWire{Kind: "del", Group: g.ID}))
	list, _ := c.f.List()
	assert.Len(t, list, 1)

	require.NoError(t, a.f.Delete(g.ID))
	for _, n := range []*testNode{a, b, c} {
		list, _ := n.f.List()
		assert.Empty(t, list, "the group is gone")
		msgs, err := n.f.Messages(g.ID, 0)
		assert.Error(t, err)
		assert.Empty(t, msgs)
	}
	assert.Contains(t, c.eventKinds(), "removed")
	var reason string
	c.mu.Lock()
	for _, e := range c.events {
		if e.Kind == "removed" {
			reason = e.Reason
		}
	}
	c.mu.Unlock()
	assert.Equal(t, "deleted", reason)
}

func TestGroupsFacade_CallEndsWhenTheLastPersonLeaves(t *testing.T) {
	h := newHub()
	a, b, c := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)
	_, _ = c.f.Join(invite)

	call, err := a.f.StartCall(g.ID, "")
	require.NoError(t, err)
	_, ok := b.f.ActiveCall(g.ID, "")
	require.True(t, ok)

	require.NoError(t, a.f.EndCall(g.ID, "", call.CallID))
	for _, n := range []*testNode{a, b, c} {
		_, ok := n.f.ActiveCall(g.ID, "")
		assert.False(t, ok, "the call is over for everybody")
	}
	assert.Contains(t, b.eventKinds(), "callend")
	assert.Error(t, a.f.EndCall(g.ID, "", ""), "without an id there is nothing to end once the call is gone")

	// an end message for another call does not end the current one
	call2, _ := a.f.StartCall(g.ID, "")
	b.f.handle(a.id, encodeWire(groupWire{Kind: "callend", Group: g.ID, CallID: "some-old-call"}))
	active, ok := b.f.ActiveCall(g.ID, "")
	require.True(t, ok)
	assert.Equal(t, call2.CallID, active.CallID)

	// somebody outside the group cannot end it
	outsider := h.node(t, newMemStore())
	b.f.handle(outsider.id, encodeWire(groupWire{Kind: "callend", Group: g.ID, CallID: call2.CallID}))
	_, ok = b.f.ActiveCall(g.ID, "")
	assert.True(t, ok)
}

func TestGroupsFacade_NewcomerGetsTheEarlierMessages(t *testing.T) {
	h := newHub()
	a, b := h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)
	_, _ = a.f.SendMessage(g.ID, "first")
	_, _ = b.f.SendMessage(g.ID, "second")
	_, _ = a.f.SendMessage(g.ID, "third")
	time.Sleep(5 * time.Millisecond) // the newcomer joins strictly after these

	joiner := h.node(t, newMemStore())
	inviteB, _ := b.f.Invite(g.ID)
	_, err := joiner.f.Join(inviteB)
	require.NoError(t, err)

	assert.Equal(t, []string{"first", "second", "third"}, texts(t, joiner, g.ID), "in the original order")
	msgs, _ := joiner.f.Messages(g.ID, 0)
	assert.Equal(t, b.id.String(), msgs[1].Sender, "with the right authors")
	assert.False(t, msgs[1].IsMine)
	assert.Contains(t, joiner.eventKinds(), "history")

	// the same history again (a second admin, a retry) adds nothing
	joiner.f.handle(a.id, encodeWire(groupWire{Kind: "hist", Group: g.ID, History: []wireHistory{{ID: msgs[0].ID, Sender: a.id.String(), Text: "first", TS: msgs[0].Timestamp}}}))
	assert.Len(t, texts(t, joiner, g.ID), 3)
}

func TestGroupsFacade_HistoryOnlyFromAdminsAndOnlyOlderThanTheJoin(t *testing.T) {
	h := newHub()
	a, b, c := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)
	_, _ = c.f.Join(invite)

	// a plain member cannot hand over history
	b.f.handle(c.id, encodeWire(groupWire{Kind: "hist", Group: g.ID, History: []wireHistory{{ID: "h1", Sender: a.id.String(), Text: "forged", TS: 1}}}))
	assert.NotContains(t, texts(t, b, g.ID), "forged")

	// an admin cannot slip in a message from the future of our own joining, or from a stranger
	require.NoError(t, a.f.SetRole(g.ID, c.id.String(), "admin"))
	future := time.Now().Add(time.Hour).UnixMilli()
	stranger := h.node(t, newMemStore())
	b.f.handle(c.id, encodeWire(groupWire{Kind: "hist", Group: g.ID, History: []wireHistory{
		{ID: "h2", Sender: a.id.String(), Text: "from the future", TS: future},
		{ID: "h3", Sender: stranger.id.String(), Text: "from a stranger", TS: 1},
		{ID: "h4", Sender: a.id.String(), Text: "fine", TS: 1},
	}}))
	got := texts(t, b, g.ID)
	assert.Equal(t, []string{"fine"}, got)
}

func TestGroupsFacade_AddedMemberAsksForHistory(t *testing.T) {
	h := newHub()
	a, b := h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	_, _ = a.f.SendMessage(g.ID, "before you")
	time.Sleep(5 * time.Millisecond)

	b.known[a.id] = true
	require.NoError(t, a.f.AddMember(g.ID, b.pubHex))
	assert.Equal(t, []string{"before you"}, texts(t, b, g.ID))
}

func countEvents(n *testNode, kind string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	k := 0
	for _, ev := range n.events {
		if ev.Kind == kind {
			k++
		}
	}
	return k
}

// A group call stays visible while its members keep announcing it, repeats do not ring again, a
// member who missed the start still sees it (without ringing), and a stale announcement is ignored.
func TestGroupsFacade_CallStaysLiveWhileAnnounced(t *testing.T) {
	h := newHub()
	a, b := h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)

	call, err := a.f.StartCall(g.ID, "")
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(b, "call"))

	// the same call announced again: still one ring
	b.f.handle(a.id, encodeWire(groupWire{Kind: "callalive", Group: g.ID, CallID: call.CallID, Coord: call.Coordinator, TS: time.Now().Unix()}))
	assert.Equal(t, 1, countEvents(b, "call"))
	assert.Equal(t, 0, countEvents(b, "callactive"), "a call already known and live is not news")

	// a newcomer who missed the start learns of the call from a keep-alive: shown, not rung
	c := h.node(t, newMemStore())
	inviteB, _ := b.f.Invite(g.ID)
	_, _ = c.f.Join(inviteB)
	c.f.handle(a.id, encodeWire(groupWire{Kind: "callalive", Group: g.ID, CallID: call.CallID, Coord: call.Coordinator, TS: time.Now().Unix()}))
	assert.Equal(t, 0, countEvents(c, "call"))
	assert.Equal(t, 1, countEvents(c, "callactive"))
	active, ok := c.f.ActiveCall(g.ID, "")
	require.True(t, ok)
	assert.Equal(t, call.CallID, active.CallID)

	// an announcement that waited in a queue for long does not ring and does not resurrect
	stale := h.node(t, newMemStore())
	inviteC, _ := c.f.Invite(g.ID)
	_, _ = stale.f.Join(inviteC)
	stale.f.handle(a.id, encodeWire(groupWire{Kind: "call", Group: g.ID, CallID: call.CallID, Coord: call.Coordinator, TS: time.Now().Add(-10 * time.Minute).Unix()}))
	assert.Equal(t, 0, countEvents(stale, "call"))
	_, ok = stale.f.ActiveCall(g.ID, "")
	assert.False(t, ok)

	// nobody announces it any more: it is over
	b.f.mu.Lock()
	cc := b.f.calls[g.ID]
	cc.LastSeen = time.Now().Add(-callAliveFor - time.Second).Unix()
	b.f.calls[g.ID] = cc
	b.f.mu.Unlock()
	_, ok = b.f.ActiveCall(g.ID, "")
	assert.False(t, ok, "a call nobody announced for callAliveFor is over")

	// KeepCallAlive from a member brings it back for the others
	require.NoError(t, a.f.KeepCallAlive(g.ID, "", call.CallID, call.Coordinator))
	_, ok = b.f.ActiveCall(g.ID, "")
	assert.True(t, ok)
}

// A call in a voice room does not ring the members: they see it going on in that room, apart from
// the group's own call, and it ends on its own.
func TestGroupsFacade_VoiceRoomCallDoesNotRing(t *testing.T) {
	h := newHub()
	a, b := h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)

	room, err := a.f.StartCall(g.ID, "lounge")
	require.NoError(t, err)
	b.mu.Lock()
	var rang, seen bool
	for _, ev := range b.events {
		rang = rang || ev.Kind == "call"
		seen = seen || (ev.Kind == "callactive" && ev.Room == "lounge" && ev.CallID == room.CallID)
	}
	b.mu.Unlock()
	assert.False(t, rang, "a voice room must not ring")
	assert.True(t, seen, "the members see the room's call")
	_, groupCall := b.f.ActiveCall(g.ID, "")
	assert.False(t, groupCall, "the room's call is not the group's call")
	assert.Equal(t, room.CallID, b.f.ActiveRoomCalls(g.ID)["lounge"].CallID)

	require.NoError(t, a.f.EndCall(g.ID, "lounge", room.CallID))
	assert.Empty(t, b.f.ActiveRoomCalls(g.ID))
}

// A build from before voice rooms reads "call" as the group's own call and rings, whatever room
// it names. A voice room's call is announced under kinds such a build does not know, and a room
// kind that names no room is dropped.
func TestGroupsFacade_VoiceRoomCallIsNotAGroupCallOnTheWire(t *testing.T) {
	h := newHub()
	a, b := h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)

	h.mu.Lock()
	h.offline[b.id] = true
	h.mu.Unlock()
	room, err := a.f.StartCall(g.ID, "lounge")
	require.NoError(t, err)
	require.NoError(t, a.f.KeepCallAlive(g.ID, "lounge", room.CallID, room.Coordinator))
	h.mu.Lock()
	var kinds []string
	for _, m := range h.waiting[b.id] {
		var w groupWire
		if strings.HasPrefix(m.text, wirePrefix) && json.Unmarshal([]byte(strings.TrimPrefix(m.text, wirePrefix)), &w) == nil && w.CallID != "" {
			kinds = append(kinds, w.Kind)
		}
	}
	h.mu.Unlock()
	assert.Equal(t, []string{"rcall", "rcallalive"}, kinds)

	b.f.handle(a.id, encodeWire(groupWire{Kind: "rcall", Group: g.ID, CallID: "x", Coord: a.id.String()}))
	_, groupCall := b.f.ActiveCall(g.ID, "")
	assert.False(t, groupCall, "a room kind without a room is no call at all")
}

// Who is in a voice room: everybody who announced it lately, until they say they left.
func TestGroupsFacade_VoiceRoomShowsWhoIsIn(t *testing.T) {
	h := newHub()
	a, b := h.node(t, newMemStore()), h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	invite, _ := a.f.Invite(g.ID)
	_, _ = b.f.Join(invite)

	room, err := a.f.StartCall(g.ID, "lounge")
	require.NoError(t, err)
	require.NoError(t, b.f.KeepCallAlive(g.ID, "lounge", room.CallID, room.Coordinator))
	assert.ElementsMatch(t, []string{a.id.String(), b.id.String()}, a.f.ActiveRoomCalls(g.ID)["lounge"].Members)

	b.f.LeaveCall(g.ID, "lounge", room.CallID)
	assert.Equal(t, []string{a.id.String()}, a.f.ActiveRoomCalls(g.ID)["lounge"].Members)
}
