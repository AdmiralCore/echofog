package node

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/groups/senderkeys"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failStore fails writes (and reads, when asked) under a prefix.
type failStore struct {
	*memStore
	failSet, failFind string
}

func (s *failStore) Set(k, v []byte) error {
	if s.failSet != "" && strings.HasPrefix(string(k), s.failSet) {
		return errors.New("disk full")
	}
	return s.memStore.Set(k, v)
}

func (s *failStore) FindValues(prefix []byte, limit int) ([]storage.Entry, error) {
	if s.failFind != "" && strings.HasPrefix(string(prefix), s.failFind) {
		return nil, errors.New("disk broken")
	}
	return s.memStore.FindValues(prefix, limit)
}

// a sealed message is only sent once its chain is saved; when saving fails it goes the old way
func TestSealFallsBackWhenKeysCannotBeSaved(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 2)
	a, b := ns[0], ns[1]
	a.say(t, g, "warm up")
	fs := &failStore{memStore: a.store, failSet: groupKeysPrefix}
	a.f.store = fs
	before := m.kind("smsg")
	a.say(t, g, "keys cannot be saved")
	assert.Equal(t, before, m.kind("smsg"), "nothing sealed with an unsaved chain")
	assert.Contains(t, b.texts(t, g), "keys cannot be saved", "it went the old way")

	// no identity: no epoch can be made
	a.f.mu.Lock()
	sign := a.f.sign
	a.f.sign = nil
	_, err := a.f.newEpoch(g)
	a.f.sign = sign
	assert.Error(t, err)
	assert.Nil(t, a.f.rotate(g), "a failed epoch hands nothing out")
	_, err = a.f.newEpoch("no such group")
	assert.Error(t, err)
	a.f.mu.Unlock()

	// a broken envelope store: nothing cached is listed, pruning and forgetting cope
	fs.failSet, fs.failFind = "", groupEnvPrefix
	a.f.mu.Lock()
	assert.Empty(t, a.f.cachedEnvs(g))
	a.f.pruneEnvs(g)
	a.f.forgetKeys(g)
	a.f.mu.Unlock()
}

func TestEnvelopesThatAreNotTaken(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b, c := ns[0], ns[1], ns[2]
	a.say(t, g, "warm up")
	var good *senderkeys.Envelope
	for _, ce := range b.f.cachedEnvs(g) {
		if ce.Env.Sender == a.id.String() {
			good = ce.Env
		}
	}
	require.NotNil(t, good)
	take := func(origin *meshNode, v *senderkeys.Envelope) []func() {
		b.f.mu.Lock()
		defer b.f.mu.Unlock()
		return b.f.takeEnvelope(origin.id, g, v)
	}
	clone := func(mod func(v *senderkeys.Envelope)) *senderkeys.Envelope {
		v := *good
		mod(&v)
		return &v
	}
	assert.Nil(t, take(c, nil))
	assert.Nil(t, take(c, clone(func(v *senderkeys.Envelope) { v.Group = "other" })))
	assert.Nil(t, take(c, clone(func(v *senderkeys.Envelope) { v.Sender = "zz" })), "a sender that is no peer id")
	assert.Nil(t, take(c, good), "already had it")
	b.f.mu.Lock()
	assert.Nil(t, b.f.takeEnvelope(c.id, "no such group", good))
	b.f.mu.Unlock()

	// a sealed message from a restricted member is not taken
	require.NoError(t, a.f.SetRole(g, c.id.String(), "restricted"))
	m.settle()
	kr := c.keyring(g)
	cc := senderkeys.Content{Group: g, ID: "r1", TS: time.Now().UnixMilli(), Sender: c.id.String(), Text: "muted"}
	require.NoError(t, cc.Sign(func(d []byte) ([]byte, error) { return ed25519.Sign(c.priv, d), nil }))
	plain, _ := json.Marshal(cc)
	v, err := kr.Seal(plain)
	require.NoError(t, err)
	assert.Nil(t, take(c, v))

	// content that is not what the envelope says, or not content at all
	seal := func(n *meshNode, body []byte) *senderkeys.Envelope {
		n.f.mu.Lock()
		defer n.f.mu.Unlock()
		v, err := n.f.keyring(g).Seal(body)
		require.NoError(t, err)
		return v
	}
	assert.Nil(t, take(a, seal(a, []byte("not json"))))
	wrongGroup := senderkeys.Content{Group: "other", ID: "w1", TS: 1, Sender: a.id.String(), Text: "x"}
	_ = wrongGroup.Sign(func(d []byte) ([]byte, error) { return ed25519.Sign(a.priv, d), nil })
	raw, _ := json.Marshal(wrongGroup)
	assert.Nil(t, take(a, seal(a, raw)))
	badExtra := senderkeys.Content{Group: g, ID: "w2", TS: 1, Sender: a.id.String(), Text: "file", Extra: []byte("{broken")}
	_ = badExtra.Sign(func(d []byte) ([]byte, error) { return ed25519.Sign(a.priv, d), nil })
	raw, _ = json.Marshal(badExtra)
	assert.Nil(t, take(a, seal(a, raw)))
	// an envelope its sender's chain is already past
	old := clone(func(v *senderkeys.Envelope) {})
	b.f.mu.Lock()
	_ = b.f.store.Delete(envKey(g, old)) // forget we had it, so only the chain stops it
	b.f.mu.Unlock()
	assert.Nil(t, take(a, old))

	// a pending envelope whose relay is no peer id stays pending
	b.f.mu.Lock()
	raw, _ = json.Marshal(cachedEnv{Env: good, From: "zz", Pending: true})
	_ = b.f.store.Set([]byte(groupEnvPrefix+g+"/zz"), raw)
	assert.Empty(t, b.f.openPending(g))
	b.f.mu.Unlock()
}

// a group file offer travels sealed too: the file metadata is signed along
func TestSealedFileOffer(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 2)
	a, b := ns[0], ns[1]
	a.say(t, g, "warm up")
	var offered []wireFile
	b.f.onFile = func(_ types.PeerID, _ string, wf wireFile) { offered = append(offered, wf) }
	_, err := a.f.post(g, "notes.pdf", &wireFile{ID: "f1", Name: "notes.pdf", Size: 10, Root: "r", Key: "k"})
	require.NoError(t, err)
	m.settle()
	require.Len(t, offered, 1)
	assert.Equal(t, "f1", offered[0].ID)
	list, _ := b.f.Messages(g, 0)
	var withFile *GroupMessageDTO
	for i := range list {
		if list[i].File != nil {
			withFile = &list[i]
		}
	}
	require.NotNil(t, withFile)
	assert.True(t, withFile.Verified)
}

func TestSyncAnswersAndRequestsAreBounded(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b, c := ns[0], ns[1], ns[2]
	for i := 0; i < 3; i++ {
		a.say(t, g, "x")
		b.say(t, g, "y")
	}
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	assert.Nil(t, a.f.answerSync(g, a.id, nil, nil), "not to ourselves")
	stranger := m.add(t)
	assert.Nil(t, a.f.answerSync(g, stranger.id, nil, nil), "not to a stranger")
	var ids []senderkeys.EpochID
	for _, e := range a.f.keyring(g).List() {
		ids = append(ids, e.ID())
	}
	after := a.f.answerSync(g, c.id, nil, ids)
	assert.Len(t, after, 1, "c gets the envelopes it lacks in one go")
	assert.Nil(t, a.f.answerSync(g, c.id, nil, ids), "and not again within syncPause")
	// b asks having nothing of an epoch: it gets the epoch, and only others' envelopes
	a.f.paced = map[string]time.Time{}
	after = a.f.answerSync(g, b.id, nil, nil)
	assert.NotEmpty(t, after)

	assert.Nil(t, a.f.requestSync(g, a.id), "no request to ourselves")
	assert.Nil(t, a.f.requestKeys(g, a.id))
	a.f.paced = map[string]time.Time{}
	assert.Len(t, a.f.requestSync(g, b.id), 1)
	assert.Nil(t, a.f.requestSync(g, b.id), "paced")
	assert.Len(t, a.f.requestKeys(g, b.id), 1)
	assert.Nil(t, a.f.requestKeys(g, b.id), "paced")
	// without any epoch, a sync request is a key request
	a.f.paced = map[string]time.Time{}
	a.f.keyrings[g] = senderkeys.NewKeyring(g, a.id.String())
	assert.Len(t, a.f.requestSync(g, c.id), 1)
	assert.Nil(t, a.f.sendEpochs(g, c.id), "nothing to hand out")
}

func TestTaintAndRotationRules(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, b := ns[0], ns[1]
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	kr := a.f.keyring(g)
	cur := kr.Current()
	// a member list with junk in it is not taken as somebody gone
	cur.Members = append(cur.Members, "zz")
	assert.False(t, a.f.tainted(g))
	assert.True(t, a.f.tainted(g, departed{b.id.String(), time.Now().UnixMilli()}), "b joined after the epoch was made")
	assert.False(t, a.f.tainted(g, departed{"x", 1}), "somebody gone before the epoch was made")
	// a plain member never rotates, even as the actor
	b.f.mu.Lock()
	assert.Nil(t, b.f.maybeRotate(g, true))
	b.f.mu.Unlock()
	empty := senderkeys.NewKeyring("none", a.id.String())
	a.f.keyrings["none"] = empty
	assert.False(t, a.f.tainted("none"))
}

func TestSyncLoopRunsAndStops(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 2)
	_ = g
	syncFirstDelay = time.Millisecond
	defer func() { syncFirstDelay = 20 * time.Second }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ns[0].f.syncLoop(ctx); close(done) }()
	require.Eventually(t, func() bool { return m.kind("sync") > 0 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not stop")
	}
}
