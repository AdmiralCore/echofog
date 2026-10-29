package groups

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/events"
)

var (
	ErrGroupNotFound      = errors.New("group not found")
	ErrGroupAlreadyExists = errors.New("group already exists")
)

// GroupState tracks in-memory state of an active group.
type GroupState struct {
	mu         sync.RWMutex
	Genesis    *GroupGenesisManifest
	Members    map[types.PeerID]MemberProfile
	Bans       map[types.PeerID]string // PeerID -> Ban reason
	Tombstones map[[16]byte]*MessageTombstone
	ActionLog  []*GroupAction
}

// GroupManager coordinates group creation, RBAC authorization, bans, and cryptographic key eviction.
type GroupManager struct {
	mu        sync.RWMutex
	groups    map[string]*GroupState
	keyEngine *KeyRotationEngine
	emitter   events.Emitter
	log       *slog.Logger
	roles     atomic.Pointer[RoleResolver]
}

// RoleResolver resolves a role's name in a group: as the group defines it (its log), or built in.
type RoleResolver func(groupID, name string) (Role, bool)

// SetRoleResolver lets the groups' own role definitions count (see RefreshRoles).
func (m *GroupManager) SetRoleResolver(r RoleResolver) { m.roles.Store(&r) }

// resolveRole resolves a role's name in a group: by the resolver, else the built-in roles.
func (m *GroupManager) resolveRole(groupID, name string) (Role, error) {
	if r := m.roles.Load(); r != nil {
		if role, ok := (*r)(groupID, name); ok {
			return role, nil
		}
	}
	return ResolveRole(name)
}

// RefreshRoles takes the group's present role definitions into its members' roles: a role whose
// permissions changed changes for everyone who holds it, and the members of a role that went get
// the group's default one.
func (m *GroupManager) RefreshRoles(groupID string) {
	st, err := m.state(groupID)
	if err != nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	def := RoleMember
	if st.Genesis != nil && st.Genesis.Type == GroupTypePublicChannel {
		def = RoleChannelSubscriber
	}
	if r, err := m.resolveRole(groupID, def.Name); err == nil {
		def = r
	}
	for id, prof := range st.Members {
		r, err := m.resolveRole(groupID, prof.Role.Name)
		if err != nil {
			r = def
		}
		if r != prof.Role {
			prof.Role = r
			st.Members[id] = prof
		}
	}
}

// NewGroupManager initializes the decentralized group manager.
func NewGroupManager() *GroupManager {
	return &GroupManager{
		groups:    make(map[string]*GroupState),
		keyEngine: NewKeyRotationEngine(),
		log:       slog.Default().With("component", "groups"),
	}
}

// CreateGroup creates a new signed genesis manifest and initializes the group with the Creator as Owner.
func (m *GroupManager) CreateGroup(title, description string, gType GroupType, creatorPriv ed25519.PrivateKey) (*GroupGenesisManifest, error) {
	if len(creatorPriv) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key size")
	}

	creatorPub := creatorPriv.Public().(ed25519.PublicKey)
	var creatorPubIdent types.IdentityPublicKey
	copy(creatorPubIdent[:], creatorPub[:32])
	creatorID := IDOfKey(creatorPubIdent) // VerifyGenesis on other nodes requires the hash of the key

	var salt [16]byte
	if _, err := io.ReadFull(rand.Reader, salt[:]); err != nil {
		return nil, err
	}

	groupID := fmt.Sprintf("fog_grp_%s_%s", hex.EncodeToString(creatorID[:6]), hex.EncodeToString(salt[:4]))

	genesis := &GroupGenesisManifest{
		GroupID:     groupID,
		Title:       title,
		Description: description,
		Type:        gType,
		Creator:     creatorID,
		CreatorPub:  creatorPubIdent,
		CreatedAt:   time.Now().UnixMilli(),
		Salt:        salt,
	}

	digest := genesis.CanonicalHash()
	sig := ed25519.Sign(creatorPriv, digest)
	copy(genesis.Signature[:], sig)

	state := &GroupState{
		Genesis:    genesis,
		Members:    make(map[types.PeerID]MemberProfile),
		Bans:       make(map[types.PeerID]string),
		Tombstones: make(map[[16]byte]*MessageTombstone),
		ActionLog:  make([]*GroupAction, 0),
	}

	// Creator is automatically Owner
	state.Members[creatorID] = MemberProfile{
		PeerID:   creatorID,
		PubKey:   creatorPubIdent,
		Role:     RoleOwner,
		JoinedAt: genesis.CreatedAt,
	}

	m.mu.Lock()
	m.groups[groupID] = state
	m.mu.Unlock()

	// Generate initial encryption key (Epoch 1)
	if _, err := m.keyEngine.GenerateInitialKey(groupID); err != nil {
		// the group exists but has no epoch key: members cannot be given one until a rotation
		m.logger().Warn("Could not create the group key", "group_id", groupID, "err", err)
	}

	m.logOp("create", groupID, nil, "type", gType, "creator", shortPeer(creatorID))
	return genesis, nil
}

// JoinGroup adds a peer to the group as Member (or Subscriber for channels).
func (m *GroupManager) JoinGroup(groupID string, member types.PeerID, pubKey types.IdentityPublicKey) (err error) {
	defer func() { m.logOp("join", groupID, err, "member", shortPeer(member)) }()
	m.mu.RLock()
	state, exists := m.groups[groupID]
	m.mu.RUnlock()

	if !exists {
		return ErrGroupNotFound
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if _, isBanned := state.Bans[member]; isBanned {
		return ErrMemberAlreadyBanned
	}

	initialRole := RoleMember
	if state.Genesis.Type == GroupTypePublicChannel {
		initialRole = RoleChannelSubscriber
	}

	state.Members[member] = MemberProfile{
		PeerID:   member,
		PubKey:   pubKey,
		Role:     initialRole,
		JoinedAt: time.Now().UnixMilli(),
	}
	m.emit("groups:member_joined", map[string]any{"group_id": groupID, "member": member.String(), "role": initialRole})

	return nil
}

// PromoteMember raises member rank, validating that actor outranks both the target and the new role.
func (m *GroupManager) PromoteMember(groupID string, actor types.PeerID, target types.PeerID, newRoleName string, actorPriv ed25519.PrivateKey) error {
	return m.modifyRole(groupID, actor, target, newRoleName, ActionTypePromote, actorPriv)
}

// DemoteMember lowers member rank.
func (m *GroupManager) DemoteMember(groupID string, actor types.PeerID, target types.PeerID, newRoleName string, actorPriv ed25519.PrivateKey) error {
	return m.modifyRole(groupID, actor, target, newRoleName, ActionTypeDemote, actorPriv)
}

func (m *GroupManager) modifyRole(groupID string, actor types.PeerID, target types.PeerID, newRoleName string, actionType ActionType, actorPriv ed25519.PrivateKey) (err error) {
	defer func() {
		m.logOp("role", groupID, err, "actor", shortPeer(actor), "target", shortPeer(target), "role", newRoleName, "action", actionType)
	}()
	m.mu.RLock()
	state, exists := m.groups[groupID]
	m.mu.RUnlock()

	if !exists {
		return ErrGroupNotFound
	}

	targetNewRole, err := m.resolveRole(groupID, newRoleName)
	if err != nil {
		return err
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	actorProf, ok := state.Members[actor]
	if !ok {
		return ErrMemberNotFound
	}

	targetProf, ok := state.Members[target]
	if !ok {
		return ErrMemberNotFound
	}

	// 1. Verify actor has PermManageRoles
	if !HasPermission(actorProf.Role, PermManageRoles) {
		return ErrInsufficientPermissions
	}

	// 2. Verify actor strictly outranks target
	if !CanAdminister(actorProf.Role, targetProf.Role) {
		return ErrInsufficientRank
	}

	// 3. Verify actor strictly outranks the role being granted (cannot grant rank >= own rank)
	if !CanAssignRole(actorProf.Role, targetNewRole) {
		return ErrInsufficientRank
	}

	// Record signed action
	action := &GroupAction{
		ActionID:  fmt.Sprintf("act_%d", time.Now().UnixNano()),
		GroupID:   groupID,
		Type:      actionType,
		Actor:     actor,
		ActorPub:  actorProf.PubKey,
		Target:    target,
		NewRole:   newRoleName,
		Timestamp: time.Now().UnixMilli(),
	}
	digest := action.CanonicalHash()
	copy(action.Signature[:], ed25519.Sign(actorPriv, digest))

	targetProf.Role = targetNewRole
	state.Members[target] = targetProf
	state.ActionLog = append(state.ActionLog, action)

	return nil
}

// BanMember permanently bans target and triggers Cryptographic Eviction (Epoch Key Rotation).
// Returns the newly rotated GroupEpochKey.
func (m *GroupManager) BanMember(groupID string, actor types.PeerID, target types.PeerID, reason string, actorPriv ed25519.PrivateKey) (key *GroupEpochKey, err error) {
	defer func() {
		args := []any{"actor", shortPeer(actor), "target", shortPeer(target)}
		if key != nil {
			args = append(args, "epoch", key.Epoch)
		}
		m.logOp("ban", groupID, err, args...)
	}()
	m.mu.RLock()
	state, exists := m.groups[groupID]
	m.mu.RUnlock()

	if !exists {
		return nil, ErrGroupNotFound
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	actorProf, ok := state.Members[actor]
	if !ok {
		return nil, ErrMemberNotFound
	}

	targetProf, ok := state.Members[target]
	if !ok {
		return nil, ErrMemberNotFound
	}

	// 1. Check PermBanMembers
	if !HasPermission(actorProf.Role, PermBanMembers) {
		return nil, ErrInsufficientPermissions
	}

	// 2. Check Rank hierarchy (cannot ban owner or equal/higher rank)
	if !CanAdminister(actorProf.Role, targetProf.Role) {
		return nil, ErrInsufficientRank
	}

	// Record signed ban action
	action := &GroupAction{
		ActionID:  fmt.Sprintf("act_ban_%d", time.Now().UnixNano()),
		GroupID:   groupID,
		Type:      ActionTypeBan,
		Actor:     actor,
		ActorPub:  actorProf.PubKey,
		Target:    target,
		Reason:    reason,
		Timestamp: time.Now().UnixMilli(),
	}
	digest := action.CanonicalHash()
	copy(action.Signature[:], ed25519.Sign(actorPriv, digest))

	// 3. Cryptographic Eviction: Rotate group epoch key. It comes first so that a failed rotation
	// leaves no ban behind that the target could still read through.
	newEpochKey, err := m.keyEngine.RotateKey(groupID)
	if err != nil {
		return nil, fmt.Errorf("cryptographic key rotation failed: %w", err)
	}

	// Mark target banned
	targetProf.IsBanned = true
	targetProf.BanReason = reason
	state.Members[target] = targetProf
	state.Bans[target] = reason
	state.ActionLog = append(state.ActionLog, action)

	m.emit("groups:member_evicted", map[string]any{"group_id": groupID, "target": target.String(), "reason": reason})
	m.emit("groups:epoch_rotated", map[string]any{"group_id": groupID, "epoch": newEpochKey.Epoch})
	return newEpochKey, nil
}

// UnbanMember restores a previously banned member.
func (m *GroupManager) UnbanMember(groupID string, actor types.PeerID, target types.PeerID, actorPriv ed25519.PrivateKey) (err error) {
	defer func() { m.logOp("unban", groupID, err, "actor", shortPeer(actor), "target", shortPeer(target)) }()
	m.mu.RLock()
	state, exists := m.groups[groupID]
	m.mu.RUnlock()

	if !exists {
		return ErrGroupNotFound
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	actorProf, ok := state.Members[actor]
	if !ok {
		return ErrMemberNotFound
	}

	if !HasPermission(actorProf.Role, PermBanMembers) {
		return ErrInsufficientPermissions
	}

	if _, isBanned := state.Bans[target]; !isBanned {
		return ErrMemberNotBanned
	}

	delete(state.Bans, target)
	if prof, ok := state.Members[target]; ok {
		prof.IsBanned = false
		prof.BanReason = ""
		state.Members[target] = prof
	}

	// Record signed action
	action := &GroupAction{
		ActionID:  fmt.Sprintf("act_unban_%d", time.Now().UnixNano()),
		GroupID:   groupID,
		Type:      ActionTypeUnban,
		Actor:     actor,
		ActorPub:  actorProf.PubKey,
		Target:    target,
		Timestamp: time.Now().UnixMilli(),
	}
	digest := action.CanonicalHash()
	copy(action.Signature[:], ed25519.Sign(actorPriv, digest))
	state.ActionLog = append(state.ActionLog, action)

	return nil
}

// DeleteMessage issues a signed MessageTombstone to permanently delete a message.
func (m *GroupManager) DeleteMessage(groupID string, actor types.PeerID, messageID [16]byte, actorPriv ed25519.PrivateKey) (_ *MessageTombstone, err error) {
	defer func() {
		m.logOp("delete message", groupID, err, "actor", shortPeer(actor), "message", hex.EncodeToString(messageID[:4]))
	}()
	m.mu.RLock()
	state, exists := m.groups[groupID]
	m.mu.RUnlock()

	if !exists {
		return nil, ErrGroupNotFound
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	actorProf, ok := state.Members[actor]
	if !ok {
		return nil, ErrMemberNotFound
	}

	if !HasPermission(actorProf.Role, PermDeleteMessages) {
		return nil, ErrInsufficientPermissions
	}

	tombstone := &MessageTombstone{
		MessageID: messageID,
		GroupID:   groupID,
		DeletedBy: actor,
		ActorPub:  actorProf.PubKey,
		DeletedAt: time.Now().UnixMilli(),
	}
	digest := tombstone.CanonicalHash()
	copy(tombstone.Signature[:], ed25519.Sign(actorPriv, digest))

	state.Tombstones[messageID] = tombstone
	return tombstone, nil
}

// IsMessageDeleted checks if a message has a valid tombstone.
func (m *GroupManager) IsMessageDeleted(groupID string, messageID [16]byte) bool {
	m.mu.RLock()
	state, exists := m.groups[groupID]
	m.mu.RUnlock()

	if !exists {
		return false
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	_, isTombstoned := state.Tombstones[messageID]
	return isTombstoned
}

// GetMemberRole retrieves the current role of a peer.
func (m *GroupManager) GetMemberRole(groupID string, peerID types.PeerID) (Role, error) {
	m.mu.RLock()
	state, exists := m.groups[groupID]
	m.mu.RUnlock()

	if !exists {
		return Role{}, ErrGroupNotFound
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	prof, ok := state.Members[peerID]
	if !ok {
		return Role{}, ErrMemberNotFound
	}

	return prof.Role, nil
}

// GetAuthorizedRecipients returns members eligible to receive the active epoch encryption key.
func (m *GroupManager) GetAuthorizedRecipients(groupID string) ([]MemberProfile, error) {
	m.mu.RLock()
	state, exists := m.groups[groupID]
	m.mu.RUnlock()

	if !exists {
		return nil, ErrGroupNotFound
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	var all []MemberProfile
	for _, prof := range state.Members {
		all = append(all, prof)
	}

	return FilterAuthorizedRecipients(all), nil
}

// GetCurrentEpochKey returns the active encryption key for the group.
func (m *GroupManager) GetCurrentEpochKey(groupID string) (*GroupEpochKey, bool) {
	return m.keyEngine.GetCurrentKey(groupID)
}

// SetEmitter attaches the node event bus for group governance telemetry
func (m *GroupManager) SetEmitter(e events.Emitter) {
	m.mu.Lock()
	m.emitter = e
	m.mu.Unlock()
}

func (m *GroupManager) emit(topic string, payload any) {
	m.mu.RLock()
	e := m.emitter
	m.mu.RUnlock()
	if e != nil {
		e.Emit(topic, payload)
	}
}
