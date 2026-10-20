package node

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/groups/senderkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newGroupOf(t *testing.T, m *mesh, n int) (string, []*meshNode) {
	t.Helper()
	nodes := make([]*meshNode, n)
	for i := range nodes {
		nodes[i] = m.add(t)
	}
	g, err := nodes[0].f.Create("Crew", "", "group")
	require.NoError(t, err)
	joinAll(t, m, nodes[0], g.ID, nodes[1:]...)
	return g.ID, nodes
}

func (n *meshNode) say(t *testing.T, group, text string) GroupMessageDTO {
	t.Helper()
	msg, err := n.f.SendMessage(group, text)
	require.NoError(t, err)
	n.m.settle()
	return msg
}

// once members know each other's version, messages travel sealed and arrive verified
func TestSealedMessagesBetweenCurrentMembers(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b, c := ns[0], ns[1], ns[2]
	for _, n := range ns {
		assert.Equal(t, uint64(1), n.current(g).N, "everybody holds the first epoch")
	}
	a.say(t, g, "hello")
	b.say(t, g, "hi")
	before := m.kind("smsg")
	c.say(t, g, "sealed now")
	assert.Equal(t, before+2, m.kind("smsg"), "the message went sealed to both others")
	for _, n := range ns {
		assert.ElementsMatch(t, []string{"hello", "hi", "sealed now"}, n.texts(t, g))
		list, _ := n.f.Messages(g, 0)
		for _, x := range list {
			assert.True(t, x.Verified, "%s: %q not verified", n.id.String()[:6], x.Text)
		}
	}
	assert.NotEmpty(t, b.f.cachedEnvs(g), "envelopes are kept to hand on")
}

// a kicked member gets the removal, and what is said afterwards is sealed in an epoch it never had
func TestKickReplacesTheEpoch(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 4)
	a, b, c, d := ns[0], ns[1], ns[2], ns[3]
	a.say(t, g, "warm up")
	old := c.keyring(g)
	raw, _ := json.Marshal(old)
	var stolen senderkeys.Keyring // what c could keep from before
	require.NoError(t, json.Unmarshal(raw, &stolen))

	require.NoError(t, a.f.Remove(g, c.id.String(), false))
	m.settle()
	m.tick(rekeyGap) // the group was made a moment ago: the new epoch waits for the gap
	assert.Contains(t, c.eventKinds(), "removed")
	for _, n := range []*meshNode{a, b, d} {
		cur := n.keyring(g).Current()
		require.NotNil(t, cur)
		assert.Equal(t, uint64(2), cur.N, "%s did not move to the new epoch", n.id.String()[:6])
		assert.False(t, cur.Has(c.id.String()), "the new epoch is not for the kicked member")
	}

	b.say(t, g, "after the kick")
	var env *senderkeys.Envelope
	for _, ce := range a.f.cachedEnvs(g) {
		if ce.Env.Sender == b.id.String() {
			env = ce.Env
		}
	}
	require.NotNil(t, env)
	_, err := stolen.Open(env)
	assert.True(t, errors.Is(err, senderkeys.ErrUnknownEpoch), "the kicked member opened a new message: %v", err)
	assert.NotContains(t, c.texts(t, g), "after the kick")
}

func (n *meshNode) eventKinds() []string {
	n.evMu.Lock()
	defer n.evMu.Unlock()
	out := make([]string, 0, len(n.events))
	for _, e := range n.events {
		out = append(out, e.Kind)
	}
	return out
}

// somebody who leaves cannot make the next epoch; the owner (or the epoch's maker) does
func TestLeaveIsFollowedByANewEpochFromTheOwner(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b, c := ns[0], ns[1], ns[2]
	require.NoError(t, c.f.Leave(g))
	m.settle()
	m.tick(rekeyGap)
	for _, n := range []*meshNode{a, b} {
		cur := n.keyring(g).Current()
		require.NotNil(t, cur)
		assert.Equal(t, uint64(2), cur.N)
		assert.False(t, cur.Has(c.id.String()))
	}
	assert.Empty(t, c.keyring(g).Epochs, "the one who left keeps no keys")
}

// two admins remove two members at the same moment: each makes an epoch the other's victim got;
// once both removals are known everybody ends on one epoch made for those who stay
func TestConcurrentRemovalsSettleOnOneCleanEpoch(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 6)
	owner, adm, x, y := ns[0], ns[1], ns[2], ns[3]
	require.NoError(t, owner.f.SetRole(g, adm.id.String(), "admin"))
	m.settle()

	m.paused = true
	require.NoError(t, owner.f.Remove(g, x.id.String(), false))
	require.NoError(t, adm.f.Remove(g, y.id.String(), true))
	m.paused = false
	m.settle()
	m.tick(rekeyGap)
	m.syncAll(1)
	m.tick(2 * rekeyGap) // the second replacement (both removals known) waits a gap too
	m.syncAll(1)

	stay := []*meshNode{owner, adm, ns[4], ns[5]}
	want := stay[0].current(g)
	for _, n := range stay {
		assert.Equal(t, want, n.current(g), "%s is on another epoch", n.id.String()[:6])
		cur := n.keyring(g).Current()
		assert.False(t, cur.Has(x.id.String()) || cur.Has(y.id.String()), "the settled epoch is for a removed member")
	}
	ns[4].say(t, g, "only us")
	for _, n := range stay {
		assert.Contains(t, n.texts(t, g), "only us")
	}
	assert.NotContains(t, x.texts(t, g), "only us")
	assert.NotContains(t, y.texts(t, g), "only us")
}

// b was offline while a wrote, and a went offline before b came back: c hands the message over
func TestAnotherMemberHandsOnWhatTheAuthorCouldNotDeliver(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b := ns[0], ns[1]
	a.say(t, g, "warm up")
	m.setOnline(b, false)
	a.say(t, g, "while you were away")
	m.setOnline(a, false)
	m.setOnline(b, true)
	assert.NotContains(t, b.texts(t, g), "while you were away", "the author is offline, the outbox cannot deliver")

	m.syncAll(1)
	assert.Contains(t, b.texts(t, g), "while you were away")
	list, _ := b.f.Messages(g, 0)
	for _, x := range list {
		if x.Text == "while you were away" {
			assert.Equal(t, a.id.String(), x.Sender, "the author, not the member who handed it on")
			assert.True(t, x.Verified)
		}
	}
	// a coming back later does not make it show twice
	m.setOnline(a, true)
	assert.Equal(t, 1, countOf(b.texts(t, g), "while you were away"))
}

// a member cannot pass off a message as another's: it has the epoch, but not the author's key
func TestAMemberCannotForgeAnotherMembersMessage(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b, c := ns[0], ns[1], ns[2]
	a.say(t, g, "warm up")

	// c builds a keyring that sends as a, signs with its own key
	fake := senderkeys.NewKeyring(g, a.id.String())
	for _, e := range c.keyring(g).List() {
		fake.Add(e)
	}
	content := senderkeys.Content{Group: g, ID: "forged", TS: time.Now().UnixMilli(), Sender: a.id.String(), Text: "a says pay me"}
	require.NoError(t, content.Sign(func(d []byte) ([]byte, error) { return ed25519.Sign(c.priv, d), nil }))
	plain, _ := json.Marshal(content)
	// skip past a's real messages so the index is fresh
	for i := 0; i < 5; i++ {
		_, _ = fake.Seal([]byte("x"))
	}
	env, err := fake.Seal(plain)
	require.NoError(t, err)
	b.f.handle(c.id, encodeWire(groupWire{Kind: "smsg", Group: g, Env: env, V: wireVersion}))
	assert.NotContains(t, b.texts(t, g), "a says pay me")

	// nor can a non-member hand anything on
	stranger := m.add(t)
	b.f.handle(stranger.id, encodeWire(groupWire{Kind: "envs", Group: g, Envs: []*senderkeys.Envelope{env}, V: wireVersion}))
	assert.NotContains(t, b.texts(t, g), "a says pay me")
}

// history is signed: an admin who changes it gets it dropped; unsigned history (older members)
// is still taken, marked unverified
func TestHistoryCarriesSignatures(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 2)
	a, b := ns[0], ns[1]
	b.say(t, g, "b said this")
	a.say(t, g, "a said that")
	// an extra node joins later and gets history from the owner (history is what is older than
	// the join, to the millisecond)
	time.Sleep(3 * time.Millisecond)
	n := m.add(t)
	joinAll(t, m, a, g, n)
	got, _ := n.f.Messages(g, 0)
	require.Len(t, got, 2)
	for _, x := range got {
		assert.True(t, x.Verified, "%q from history is verified", x.Text)
	}

	// a forged history chunk: b's words changed
	var sig []byte
	var ts int64
	var id string
	list, _ := a.f.Messages(g, 0)
	for _, x := range list {
		if x.Text == "b said this" {
			ts, id = x.Timestamp, x.ID
		}
	}
	for _, c := range a.f.historyChunks(g) {
		for _, h := range c.History {
			if h.ID == id {
				sig = h.Sig
			}
		}
	}
	require.NotEmpty(t, sig, "history carries the signature")
	late := m.add(t)
	joinAll(t, m, a, g, late)
	forged := groupWire{Kind: "hist", Group: g, History: []wireHistory{
		{ID: "x1", Sender: b.id.String(), Text: "b said something else", TS: ts, Sig: sig},
		{ID: "x2", Sender: b.id.String(), Text: "old client line", TS: ts}, // unsigned
	}}
	late.f.handle(a.id, encodeWire(forged))
	texts := late.texts(t, g)
	assert.NotContains(t, texts, "b said something else", "altered history is dropped")
	assert.Contains(t, texts, "old client line")
	lm, _ := late.f.Messages(g, 0)
	for _, x := range lm {
		if x.Text == "old client line" {
			assert.False(t, x.Verified, "unsigned history is shown as unverified")
		}
	}
}

// a member of the older version gets every message the old way, and its messages reach everybody
func TestOlderMembersStillTakePart(t *testing.T) {
	m := newMesh()
	a, b := m.add(t), m.add(t)
	old := m.add(t)
	old.legacy = true
	gd, err := a.f.Create("Mixed", "", "group")
	require.NoError(t, err)
	g := gd.ID
	joinAll(t, m, a, g, b, old)
	a.say(t, g, "one")
	b.say(t, g, "two")
	old.say(t, g, "from the old app")
	a.say(t, g, "three")
	for _, n := range []*meshNode{a, b, old} {
		assert.ElementsMatch(t, []string{"one", "two", "from the old app", "three"}, n.texts(t, g), n.id.String()[:6])
	}
	assert.Positive(t, m.kind("smsg"), "the current members sealed between themselves")
}

// a sealed message that comes before its epoch waits, the epoch is asked for, and then it opens
func TestEnvelopeWaitsForItsEpoch(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b := ns[0], ns[1]
	a.say(t, g, "warm up")
	// b loses its keys (as a newcomer that did not get them yet)
	b.f.mu.Lock()
	b.f.keyrings[g] = senderkeys.NewKeyring(g, b.id.String())
	_ = b.f.saveKeyring(g)
	b.f.mu.Unlock()
	for _, n := range ns {
		n.f.mu.Lock()
		n.f.paced = map[string]time.Time{} // the key request (and answer) while joining was minutes ago
		n.f.mu.Unlock()
	}
	a.say(t, g, "sealed before b has the key")
	assert.Contains(t, b.texts(t, g), "sealed before b has the key")
	assert.Positive(t, m.kind("keyreq"))
}

// a group made before sealed messages has no epoch; its owner starts one when it next writes
func TestOlderGroupGetsItsFirstEpochFromTheOwner(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	for _, n := range ns {
		n.f.mu.Lock()
		n.f.forgetKeys(g)
		n.f.mu.Unlock()
	}
	ns[1].say(t, g, "member writes first") // no epoch: the old way
	assert.Empty(t, ns[1].keyring(g).Epochs, "a member does not start the epoch")
	ns[0].say(t, g, "owner writes")
	for _, n := range ns {
		assert.Equal(t, uint64(1), n.current(g).N, "%s has the owner's first epoch", n.id.String()[:6])
		assert.ElementsMatch(t, []string{"member writes first", "owner writes"}, n.texts(t, g))
	}
}

// an epoch older than MaxEpochAge is replaced by the owner on the next message
func TestOldEpochIsReplaced(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 2)
	a := ns[0]
	a.f.mu.Lock()
	kr := a.f.keyring(g)
	cur := kr.Current()
	// made long ago (its signature no longer matters here: a holds it already)
	cur.CreatedAt = time.Now().Add(-senderkeys.MaxEpochAge - time.Hour).UnixMilli()
	a.f.mu.Unlock()
	a.say(t, g, "after a week")
	assert.Equal(t, uint64(2), ns[1].current(g).N)
	assert.Contains(t, ns[1].texts(t, g), "after a week")
}

// keys survive a restart: the sending chain goes on where it was
func TestKeysSurviveARestart(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 2)
	a, b := ns[0], ns[1]
	a.say(t, g, "before")
	a.start() // same store, new service
	a.say(t, g, "after the restart")
	assert.ElementsMatch(t, []string{"before", "after the restart"}, b.texts(t, g))
	tx := a.keyring(g).Send[a.current(g).String()]
	require.NotNil(t, tx)
	assert.Equal(t, uint32(2), tx.N)
}

// only an admin's epoch is taken, and only members get keys or answers
func TestEpochsAndAnswersOnlyWithinTheGroup(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b, c := ns[0], ns[1], ns[2]
	// c (a member) makes an epoch of its own
	e, err := senderkeys.NewEpoch(g, 9, c.id.String(), nil, time.Now(), func(d []byte) ([]byte, error) { return ed25519.Sign(c.priv, d), nil })
	require.NoError(t, err)
	b.f.handle(c.id, encodeWire(groupWire{Kind: "key", Group: g, Epoch: e, V: wireVersion}))
	assert.Equal(t, uint64(1), b.current(g).N, "a member's epoch is not taken")
	// an admin's epoch with a broken signature neither
	e2, _ := senderkeys.NewEpoch(g, 9, a.id.String(), nil, time.Now(), func(d []byte) ([]byte, error) { return ed25519.Sign(c.priv, d), nil })
	b.f.handle(c.id, encodeWire(groupWire{Kind: "key", Group: g, Epoch: e2, V: wireVersion}))
	assert.Equal(t, uint64(1), b.current(g).N)
	b.f.handle(c.id, encodeWire(groupWire{Kind: "key", Group: "other", Epoch: e2, V: wireVersion}))

	stranger := m.add(t)
	sent := m.kind("key")
	a.f.handle(stranger.id, encodeWire(groupWire{Kind: "keyreq", Group: g, V: wireVersion}))
	a.f.handle(stranger.id, encodeWire(groupWire{Kind: "sync", Group: g, V: wireVersion}))
	m.settle()
	assert.Equal(t, sent, m.kind("key"), "a stranger gets no keys")
	assert.Zero(t, m.kind("envs"), "nor messages")
}

// the envelope store keeps a week and a bounded number per group
func TestEnvelopeStoreIsBounded(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 2)
	a := ns[0]
	a.say(t, g, "x")
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	kr := a.f.keyring(g)
	for i := 0; i < envKeepMax+20; i++ {
		v, err := kr.Seal([]byte("x"))
		require.NoError(t, err)
		a.f.cacheEnv(g, v, a.id, false)
	}
	a.f.pruneEnvs(g)
	assert.Len(t, a.f.cachedEnvs(g), envKeepMax)
	// and old ones go
	for _, e := range a.f.cachedEnvs(g)[:10] {
		c := cachedEnv{Env: e.Env, From: e.From, At: time.Now().Add(-envKeepFor - time.Hour).UnixMilli()}
		raw, _ := json.Marshal(c)
		_ = a.f.store.Set(envKey(g, e.Env), raw)
	}
	a.f.pruneEnvs(g)
	assert.Len(t, a.f.cachedEnvs(g), envKeepMax-10)
	a.f.forgetKeys(g)
	assert.Empty(t, a.f.cachedEnvs(g))
}

func TestBatchesStayUnderTheLimit(t *testing.T) {
	var envs []*senderkeys.Envelope
	for i := 0; i < 200; i++ {
		envs = append(envs, &senderkeys.Envelope{CT: make([]byte, 1000), Nonce: make([]byte, 24)})
	}
	bs := batchEnvs(envs)
	total := 0
	for _, b := range bs {
		assert.LessOrEqual(t, len(b), maxEnvsInMsg)
		size := 0
		for _, v := range b {
			size += len(v.CT) + len(v.Nonce) + 200
		}
		assert.LessOrEqual(t, size, syncBatch)
		total += len(b)
	}
	assert.Equal(t, 200, total)
	assert.Empty(t, batchEnvs(nil))
}

// a member who was away hears the second handover of the group before the first: the second waits
// and is applied once the first arrives, so everybody ends with the same owner
func TestOwnerChangesArrivingOutOfOrder(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 4)
	a, b, c, late := ns[0], ns[1], ns[2], ns[3]
	require.NoError(t, a.f.SetRole(g, b.id.String(), "admin"))
	require.NoError(t, a.f.SetRole(g, c.id.String(), "admin"))
	m.settle()

	m.setOnline(late, false)
	require.NoError(t, a.f.TransferOwnership(g, b.id.String())) // a -> b, held for late
	m.settle()
	m.setOnline(a, false)                                       // a is away when late comes back
	require.NoError(t, b.f.TransferOwnership(g, c.id.String())) // b -> c
	m.settle()
	m.setOnline(late, true) // gets b -> c first: b is not the owner yet in its eyes
	assert.Equal(t, "owner", roleOf2(t, late, g, a), "the second handover waits")
	m.setOnline(a, true) // now a -> b arrives, and the waiting b -> c fits
	for _, n := range ns {
		assert.Equal(t, "owner", roleOf2(t, n, g, c), "%s sees another owner", n.id.String()[:6])
	}
}
