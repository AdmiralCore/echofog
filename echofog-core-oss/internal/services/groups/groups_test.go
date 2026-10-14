package groups

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func generateTestKeyPair() (types.PeerID, types.IdentityPublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}

	var peerID types.PeerID
	var pubIdent types.IdentityPublicKey
	copy(peerID[:], pub[:32])
	copy(pubIdent[:], pub[:32])

	return peerID, pubIdent, priv
}

func TestGroups_GenesisAndSignatures(t *testing.T) {
	ownerID, ownerPub, ownerPriv := generateTestKeyPair()

	mgr := NewGroupManager()
	genesis, err := mgr.CreateGroup("Sovereign Alpha", "Privacy room", GroupTypePrivateGroup, ownerPriv)
	require.NoError(t, err)
	require.NotNil(t, genesis)

	assert.Equal(t, "Sovereign Alpha", genesis.Title)
	assert.Equal(t, ownerID, genesis.Creator)
	assert.Equal(t, ownerPub, genesis.CreatorPub)

	// Verify cryptographic signature of genesis manifest
	assert.True(t, VerifyGenesisSignature(genesis))

	// Corrupt signature check
	corruptGenesis := *genesis
	corruptGenesis.Signature[0] ^= 0xFF
	assert.False(t, VerifyGenesisSignature(&corruptGenesis))
}

func TestGroups_RBACPermissionHierarchyAndBans(t *testing.T) {
	ownerID, _, ownerPriv := generateTestKeyPair()
	bobID, bobPub, bobPriv := generateTestKeyPair()
	charlieID, charliePub, charliePriv := generateTestKeyPair()
	daveID, davePub, davePriv := generateTestKeyPair()

	mgr := NewGroupManager()
	genesis, err := mgr.CreateGroup("Echo Swarm", "Main room", GroupTypeOpenSwarm, ownerPriv)
	require.NoError(t, err)
	groupID := genesis.GroupID

	// 1. Join Bob, Charlie, Dave as Members
	require.NoError(t, mgr.JoinGroup(groupID, bobID, bobPub))
	require.NoError(t, mgr.JoinGroup(groupID, charlieID, charliePub))
	require.NoError(t, mgr.JoinGroup(groupID, daveID, davePub))

	// Verify initial roles
	role, err := mgr.GetMemberRole(groupID, ownerID)
	require.NoError(t, err)
	assert.Equal(t, RoleOwner.Rank, role.Rank)

	role, err = mgr.GetMemberRole(groupID, bobID)
	require.NoError(t, err)
	assert.Equal(t, RoleMember.Rank, role.Rank)

	// 2. Owner promotes Bob to Admin, Charlie to Moderator
	require.NoError(t, mgr.PromoteMember(groupID, ownerID, bobID, "admin", ownerPriv))
	role, err = mgr.GetMemberRole(groupID, bobID)
	require.NoError(t, err)
	assert.Equal(t, RoleAdmin.Rank, role.Rank)

	require.NoError(t, mgr.PromoteMember(groupID, ownerID, charlieID, "moderator", ownerPriv))
	role, err = mgr.GetMemberRole(groupID, charlieID)
	require.NoError(t, err)
	assert.Equal(t, RoleModerator.Rank, role.Rank)

	// 3. Security Invariant Violations:
	// Moderator Charlie tries to promote Dave (moderator lacks PermManageRoles)
	err = mgr.PromoteMember(groupID, charlieID, daveID, "moderator", charliePriv)
	assert.ErrorIs(t, err, ErrInsufficientPermissions)

	// Admin Bob has PermManageRoles, but tries to promote Dave to Admin (cannot grant rank >= own rank)
	err = mgr.PromoteMember(groupID, bobID, daveID, "admin", bobPriv)
	assert.ErrorIs(t, err, ErrInsufficientRank)

	// Moderator Charlie tries to ban Admin Bob (moderator lacks PermBanMembers)
	_, err = mgr.BanMember(groupID, charlieID, bobID, "toxic", charliePriv)
	assert.ErrorIs(t, err, ErrInsufficientPermissions)

	// Admin Bob has PermBanMembers, but tries to ban Owner (cannot moderate higher rank)
	_, err = mgr.BanMember(groupID, bobID, ownerID, "coup attempt", bobPriv)
	assert.ErrorIs(t, err, ErrInsufficientRank)

	// Member Dave tries to ban anyone (insufficient permissions)
	_, err = mgr.BanMember(groupID, daveID, charlieID, "revenge", davePriv)
	assert.ErrorIs(t, err, ErrInsufficientPermissions)

	// 4. Successful Ban of Dave by Admin Bob -> Cryptographic Eviction
	initialKey, ok := mgr.GetCurrentEpochKey(groupID)
	require.True(t, ok)
	assert.Equal(t, uint64(1), initialKey.Epoch)

	newEpochKey, err := mgr.BanMember(groupID, bobID, daveID, "spamming malware", bobPriv)
	require.NoError(t, err)
	require.NotNil(t, newEpochKey)

	// Verify epoch key rotated (Epoch 2) and key changed
	assert.Equal(t, uint64(2), newEpochKey.Epoch)
	assert.NotEqual(t, initialKey.Key, newEpochKey.Key)

	// Verify member status
	roleDave, err := mgr.GetMemberRole(groupID, daveID)
	require.NoError(t, err)
	_ = roleDave

	// 5. Verify Cryptographic Eviction: Dave is NOT in authorized recipients list!
	authorized, err := mgr.GetAuthorizedRecipients(groupID)
	require.NoError(t, err)
	var authorizedIDs []types.PeerID
	for _, a := range authorized {
		authorizedIDs = append(authorizedIDs, a.PeerID)
	}
	assert.Contains(t, authorizedIDs, ownerID)
	assert.Contains(t, authorizedIDs, bobID)
	assert.Contains(t, authorizedIDs, charlieID)
	assert.NotContains(t, authorizedIDs, daveID, "banned member Dave must be completely evicted from key distribution")

	// 6. Test Key distribution encryption via NaCl box for Charlie
	sealedKey, err := EncryptKeyForMember(newEpochKey.Key, charliePub)
	require.NoError(t, err)
	assert.NotEmpty(t, sealedKey)

	// 7. Unban Dave
	err = mgr.UnbanMember(groupID, bobID, daveID, bobPriv)
	require.NoError(t, err)

	authorizedAfter, err := mgr.GetAuthorizedRecipients(groupID)
	require.NoError(t, err)
	var authIDsAfter []types.PeerID
	for _, a := range authorizedAfter {
		authIDsAfter = append(authIDsAfter, a.PeerID)
	}
	assert.Contains(t, authIDsAfter, daveID, "unbanned member should be eligible for keys again")
}

func TestGroups_MessageTombstoneDeletion(t *testing.T) {
	ownerID, _, ownerPriv := generateTestKeyPair()
	modID, modPub, modPriv := generateTestKeyPair()
	regularID, regularPub, regularPriv := generateTestKeyPair()

	mgr := NewGroupManager()
	genesis, err := mgr.CreateGroup("Clean Chat", "No spam", GroupTypeOpenSwarm, ownerPriv)
	require.NoError(t, err)
	groupID := genesis.GroupID

	require.NoError(t, mgr.JoinGroup(groupID, modID, modPub))
	require.NoError(t, mgr.JoinGroup(groupID, regularID, regularPub))
	require.NoError(t, mgr.PromoteMember(groupID, ownerID, modID, "moderator", ownerPriv))

	var msgID [16]byte
	msgID[0] = 0xAA

	// Regular member cannot delete messages
	_, err = mgr.DeleteMessage(groupID, regularID, msgID, regularPriv)
	assert.ErrorIs(t, err, ErrInsufficientPermissions)
	assert.False(t, mgr.IsMessageDeleted(groupID, msgID))

	// Moderator deletes message successfully
	tombstone, err := mgr.DeleteMessage(groupID, modID, msgID, modPriv)
	require.NoError(t, err)
	require.NotNil(t, tombstone)

	assert.True(t, mgr.IsMessageDeleted(groupID, msgID))
	assert.True(t, VerifyTombstoneSignature(tombstone))

	// Verify action verification
	act := &GroupAction{
		ActionID:  "act_1",
		GroupID:   groupID,
		Type:      ActionTypePromote,
		Actor:     ownerID,
		ActorPub:  genesis.CreatorPub,
		Target:    modID,
		NewRole:   "moderator",
		Timestamp: 1000,
	}
	digest := act.CanonicalHash()
	copy(act.Signature[:], ed25519.Sign(ownerPriv, digest))
	assert.True(t, VerifyActionSignature(act))
}

func TestGroups_EdgeCases(t *testing.T) {
	ownerID, _, ownerPriv := generateTestKeyPair()
	user2ID, user2Pub, _ := generateTestKeyPair()

	mgr := NewGroupManager()
	genesis, err := mgr.CreateGroup("Channel One", "Public", GroupTypePublicChannel, ownerPriv)
	require.NoError(t, err)
	groupID := genesis.GroupID

	// Channel join gets subscriber role
	require.NoError(t, mgr.JoinGroup(groupID, user2ID, user2Pub))
	r, err := mgr.GetMemberRole(groupID, user2ID)
	require.NoError(t, err)
	assert.Equal(t, RoleChannelSubscriber.Name, r.Name)
	assert.False(t, HasPermission(r, PermSendMessages), "subscriber in channel cannot send messages")

	// Demote member
	require.NoError(t, mgr.PromoteMember(groupID, ownerID, user2ID, "moderator", ownerPriv))
	require.NoError(t, mgr.DemoteMember(groupID, ownerID, user2ID, "member", ownerPriv))

	// Non-existent group errors
	assert.ErrorIs(t, mgr.JoinGroup("unknown", user2ID, user2Pub), ErrGroupNotFound)
	assert.ErrorIs(t, mgr.PromoteMember("unknown", ownerID, user2ID, "admin", ownerPriv), ErrGroupNotFound)
	_, err = mgr.BanMember("unknown", ownerID, user2ID, "test", ownerPriv)
	assert.ErrorIs(t, err, ErrGroupNotFound)
	assert.ErrorIs(t, mgr.UnbanMember("unknown", ownerID, user2ID, ownerPriv), ErrGroupNotFound)
	_, err = mgr.DeleteMessage("unknown", ownerID, [16]byte{}, ownerPriv)
	assert.ErrorIs(t, err, ErrGroupNotFound)
	assert.False(t, mgr.IsMessageDeleted("unknown", [16]byte{}))
	_, err = mgr.GetMemberRole("unknown", ownerID)
	assert.ErrorIs(t, err, ErrGroupNotFound)
	_, err = mgr.GetAuthorizedRecipients("unknown")
	assert.ErrorIs(t, err, ErrGroupNotFound)

	// Unknown role error
	err = mgr.PromoteMember(groupID, ownerID, user2ID, "super_god", ownerPriv)
	assert.ErrorIs(t, err, ErrUnknownRole)

	// Unban not banned peer
	err = mgr.UnbanMember(groupID, ownerID, user2ID, ownerPriv)
	assert.ErrorIs(t, err, ErrMemberNotBanned)

	// Invalid private key size
	_, err = mgr.CreateGroup("Fail", "Fail", GroupTypeOpenSwarm, []byte("short"))
	assert.Error(t, err)
}
