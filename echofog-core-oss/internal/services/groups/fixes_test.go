package groups

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A genesis made by CreateGroup has to pass the check other nodes run on it.
func TestGroups_CreateGroupGenesisVerifies(t *testing.T) {
	ownerID, ownerPub, ownerPriv := generateTestKeyPair()
	mgr := NewGroupManager()
	g, err := mgr.CreateGroup("Verified", "", GroupTypePrivateGroup, ownerPriv)
	require.NoError(t, err)
	assert.Equal(t, IDOfKey(ownerPub), g.Creator)
	assert.Equal(t, ownerID, g.Creator)
	require.NoError(t, VerifyGenesis(g))

	other := NewGroupManager()
	require.NoError(t, other.ImportGroup(g))
	assert.True(t, other.IsOwner(g.GroupID, ownerID))
}

// The member opens the sealed epoch key with its own identity key.
func TestGroups_SealedKeyRoundTrip(t *testing.T) {
	_, pub, priv := generateTestKeyPair()
	_, _, otherPriv := generateTestKeyPair()
	key := bytes.Repeat([]byte{0x5a}, 32)

	sealed, err := EncryptKeyForMember(key, pub)
	require.NoError(t, err)
	opened, err := DecryptKeyForMember(sealed, priv)
	require.NoError(t, err)
	assert.Equal(t, key, opened)

	_, err = DecryptKeyForMember(sealed, otherPriv)
	assert.Error(t, err, "another identity must not open the box")
	_, err = DecryptKeyForMember(sealed[:40], priv)
	assert.Error(t, err)
}

// A ban received from another member evicts the target from the epoch key like a local ban.
func TestGroups_RemoteBanRotatesKey(t *testing.T) {
	ownerID, _, ownerPriv := generateTestKeyPair()
	daveID, davePub, _ := generateTestKeyPair()
	eveID, evePub, _ := generateTestKeyPair()
	mgr := NewGroupManager()
	g, err := mgr.CreateGroup("Remote ban", "", GroupTypePrivateGroup, ownerPriv)
	require.NoError(t, err)
	require.NoError(t, mgr.JoinGroup(g.GroupID, daveID, davePub))
	require.NoError(t, mgr.JoinGroup(g.GroupID, eveID, evePub))

	before, ok := mgr.GetCurrentEpochKey(g.GroupID)
	require.True(t, ok)
	require.NoError(t, mgr.ApplyRemove(g.GroupID, ownerID, daveID, true))
	after, ok := mgr.GetCurrentEpochKey(g.GroupID)
	require.True(t, ok)
	assert.Equal(t, before.Epoch+1, after.Epoch, "a remote ban must rotate the epoch key")
	assert.NotEqual(t, before.Key, after.Key)

	// a kick and a leave do not rotate
	require.NoError(t, mgr.ApplyRemove(g.GroupID, ownerID, eveID, false))
	kept, _ := mgr.GetCurrentEpochKey(g.GroupID)
	assert.Equal(t, after.Epoch, kept.Epoch)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

// When the key cannot be rotated the ban is not applied at all.
func TestGroups_BanRotationFailureLeavesNoBan(t *testing.T) {
	ownerID, _, ownerPriv := generateTestKeyPair()
	daveID, davePub, _ := generateTestKeyPair()
	mgr := NewGroupManager()
	g, err := mgr.CreateGroup("Half ban", "", GroupTypePrivateGroup, ownerPriv)
	require.NoError(t, err)
	require.NoError(t, mgr.JoinGroup(g.GroupID, daveID, davePub))

	keyRand = failingReader{}
	defer func() { keyRand = defaultKeyRand }()

	_, err = mgr.BanMember(g.GroupID, ownerID, daveID, "spam", ownerPriv)
	require.Error(t, err)

	st, err := mgr.state(g.GroupID)
	require.NoError(t, err)
	st.mu.RLock()
	defer st.mu.RUnlock()
	assert.False(t, st.Members[daveID].IsBanned)
	assert.NotContains(t, st.Bans, daveID)
	assert.Empty(t, st.ActionLog)

	// the same holds for a remote ban
	st.mu.RUnlock()
	err = mgr.ApplyRemove(g.GroupID, ownerID, daveID, true)
	st.mu.RLock()
	require.Error(t, err)
	assert.Contains(t, st.Members, daveID)
	assert.NotContains(t, st.Bans, daveID)
}
