package node

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenOf reads the token out of an invitation link.
func tokenOf(t *testing.T, invite string) *inviteToken {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(invite, inviteScheme2))
	require.NoError(t, err)
	var w groupWire
	require.NoError(t, json.Unmarshal(raw, &w))
	return w.Inv
}

// A private group lets in only who comes with a valid invitation: signed by a member who takes
// part, not run out, not altered. Knowing the group's ID, or holding an old-style link, is not
// enough any more.
func TestPrivateGroupJoinTakesAValidInvitation(t *testing.T) {
	h := newHub()
	owner, friend, stranger, member := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())
	g, err := owner.f.Create("Crew", "", "group")
	require.NoError(t, err)
	invite, err := owner.f.Invite(g.ID)
	require.NoError(t, err)
	tok := tokenOf(t, invite)
	require.NotNil(t, tok, "a private group's invitation carries a token")

	// with the invitation: in
	_, err = friend.f.Join(invite)
	require.NoError(t, err)
	assert.NotEqual(t, "", roleOf(t, owner, g.ID, friend))

	join := func(n *testNode, inv *inviteToken) string {
		owner.f.handle(n.id, encodeWire(groupWire{Kind: "join", Group: g.ID, Pub: n.pubHex, Inv: inv}))
		return roleOf(t, owner, g.ID, n)
	}
	// the group's ID and nothing else, as before
	assert.Equal(t, "", join(stranger, nil), "no invitation: refused")
	// a token altered after signing
	forged := *tok
	forged.N = strings.Repeat("0", 32)
	assert.Equal(t, "", join(stranger, &forged), "an altered token: refused")
	// one that ran out, even signed by the owner
	owner.f.mu.Lock()
	old, err := owner.f.newInviteToken(g.ID)
	require.NoError(t, err)
	old.Exp = owner.f.clock().Unix() - 60
	sig, err := owner.f.sign(inviteDigest(g.ID, old.N, old.Exp, old.Iss))
	owner.f.mu.Unlock()
	require.NoError(t, err)
	old.Sig = hex.EncodeToString(sig)
	assert.Equal(t, "", join(stranger, old), "an invitation that ran out: refused")

	// a member's invitation lets people in; once that member is removed, it does not
	minv, err := friend.f.Invite(g.ID)
	require.NoError(t, err)
	mtok := tokenOf(t, minv)
	require.NoError(t, owner.f.Remove(g.ID, friend.id.String(), false))
	assert.Equal(t, "", join(member, mtok), "the invitation of a removed member: refused")

	// a valid one still works for somebody else
	assert.NotEqual(t, "", join(stranger, tok), "the owner's invitation: in")
}

// An open room and a channel join freely, as they always did: their invitations carry no token.
func TestOpenGroupsNeedNoInvitation(t *testing.T) {
	h := newHub()
	owner, x := h.node(t, newMemStore()), h.node(t, newMemStore())
	for _, kind := range []string{"channel", "open"} {
		g, err := owner.f.Create("Open", "", kind)
		require.NoError(t, err, kind)
		inv, err := owner.f.Invite(g.ID)
		require.NoError(t, err)
		assert.Nil(t, tokenOf(t, inv), kind)
		owner.f.handle(x.id, encodeWire(groupWire{Kind: "join", Group: g.ID, Pub: x.pubHex}))
		assert.NotEqual(t, "", roleOf(t, owner, g.ID, x), kind+": joins without a token")
	}
}

// An invitation the group's log has used up or revoked lets nobody else in, though its token is
// still signed and in time; an https or echofog://group/ form of an invitation joins like the
// invitation itself; and an invitation names only those who let newcomers in, not every member.
func TestManagedInvitationsAndTheirForms(t *testing.T) {
	h := newHub()
	owner, a, b, c := h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore()), h.node(t, newMemStore())
	g, err := owner.f.Create("Crew", "", "group")
	require.NoError(t, err)
	invite, err := owner.f.Invite(g.ID)
	require.NoError(t, err)
	tok := tokenOf(t, invite)

	state := convlog.Invite{N: tok.N, Exp: tok.Exp, Uses: 1}
	owner.f.inviteState = func(_, n string) (convlog.Invite, bool) { return state, n == tok.N }
	join := func(n *testNode) string {
		owner.f.handle(n.id, encodeWire(groupWire{Kind: "join", Group: g.ID, Pub: n.pubHex, Inv: tok}))
		return roleOf(t, owner, g.ID, n)
	}
	assert.NotEqual(t, "", join(a), "an invitation with a use left lets in")
	state.Used = 1
	assert.Equal(t, "", join(b), "a used-up invitation lets nobody else in")
	state.Used, state.Revoked = 0, true
	assert.Equal(t, "", join(b), "a revoked invitation lets nobody in")

	// the forms of the link
	web := GroupWebLink(invite)
	require.True(t, strings.HasPrefix(web, "https://"), web)
	assert.Equal(t, invite, groupInviteText("join us: "+web))
	assert.Equal(t, invite, groupInviteText("echofog://group/"+strings.TrimPrefix(invite, inviteScheme2)))
	_, err = c.f.Join(web)
	require.NoError(t, err, "the https form joins")

	// many plain members: the invitation stays short
	for i := 0; i < 30; i++ {
		n := h.node(t, newMemStore())
		require.NoError(t, owner.f.AddMember(g.ID, n.pubHex))
	}
	owner.f.mu.Lock()
	listed := owner.f.inviteMembers(g.ID)
	owner.f.mu.Unlock()
	assert.Len(t, listed, 1, "only the owner lets newcomers in")
}

// With an invitation that asks for approval the newcomer is not let in at once: a request waits
// with whoever may let members in, who accepts it (the newcomer is in) or not.
func TestApprovalInvitationsWaitForAnAnswer(t *testing.T) {
	h := newHub()
	owner, x := h.node(t, newMemStore()), h.node(t, newMemStore())
	g, err := owner.f.Create("Crew", "", "group")
	require.NoError(t, err)
	invite, err := owner.f.Invite(g.ID)
	require.NoError(t, err)
	tok := tokenOf(t, invite)
	owner.f.inviteState = func(_, n string) (convlog.Invite, bool) {
		return convlog.Invite{N: tok.N, Exp: tok.Exp, Approval: true, Label: "stream"}, n == tok.N
	}
	owner.f.handle(x.id, encodeWire(groupWire{Kind: "join", Group: g.ID, Pub: x.pubHex, Inv: tok}))
	assert.Equal(t, "", roleOf(t, owner, g.ID, x), "not let in before an answer")
	owner.f.mu.Lock()
	reqs := owner.f.requestsLocked(g.ID)
	owner.f.mu.Unlock()
	require.Len(t, reqs, 1)
	assert.Equal(t, "stream", reqs[0].Label)

	pub, ok := parsePub(reqs[0].Pub)
	require.True(t, ok)
	owner.f.mu.Lock()
	after := owner.f.admitLocked(g.ID, x.id, reqs[0].Pub, pub, reqs[0].Via)
	owner.f.mu.Unlock()
	for _, fn := range after {
		fn()
	}
	assert.NotEqual(t, "", roleOf(t, owner, g.ID, x), "accepted: in")
	owner.f.mu.Lock()
	assert.Empty(t, owner.f.requestsLocked(g.ID), "the request went")
	owner.f.mu.Unlock()
}
