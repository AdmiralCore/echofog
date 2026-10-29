package groups

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Changes made by other members arrive as messages from the end-to-end encrypted session with that
// member, so who did it is known. The methods below apply such a change to the local state after
// the checks a local change gets: the actor must hold the permission and outrank the target.

var ErrNotAllowed = errors.New("not allowed")

func (m *GroupManager) state(groupID string) (*GroupState, error) {
	m.mu.RLock()
	st, ok := m.groups[groupID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrGroupNotFound
	}
	return st, nil
}

// IDOfKey is the peer ID that belongs to an identity key.
func IDOfKey(pub types.IdentityPublicKey) types.PeerID { return sha256.Sum256(pub[:]) }

// ApplyRole sets the role of target on the word of actor.
func (m *GroupManager) ApplyRole(groupID string, actor, target types.PeerID, roleName string) (err error) {
	defer func() {
		m.logOp("remote role", groupID, err, "actor", shortPeer(actor), "target", shortPeer(target), "role", roleName)
	}()
	role, err := m.resolveRole(groupID, roleName)
	if err != nil {
		return err
	}
	st, err := m.state(groupID)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	a, ok := st.Members[actor]
	if !ok {
		return ErrMemberNotFound
	}
	t, ok := st.Members[target]
	if !ok {
		return ErrMemberNotFound
	}
	if !HasPermission(a.Role, PermManageRoles) {
		return ErrInsufficientPermissions
	}
	if !CanAdminister(a.Role, t.Role) || !CanAssignRole(a.Role, role) {
		return ErrInsufficientRank
	}
	t.Role = role
	st.Members[target] = t
	m.emit("groups:role_changed", map[string]any{"group_id": groupID, "member": target.String(), "role": role.Name})
	return nil
}

// ApplyRemove takes target out of the group on the word of actor: a kick needs the permission to
// kick, a ban the permission to ban, and in both the actor has to outrank the target. A member may
// always remove themselves (leave).
func (m *GroupManager) ApplyRemove(groupID string, actor, target types.PeerID, ban bool) (err error) {
	defer func() {
		op := "remote kick"
		switch {
		case actor == target:
			op = "remote leave"
		case ban:
			op = "remote ban"
		}
		m.logOp(op, groupID, err, "actor", shortPeer(actor), "target", shortPeer(target))
	}()
	st, err := m.state(groupID)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.Members[target]
	if !ok {
		return ErrMemberNotFound
	}
	if actor != target {
		a, ok := st.Members[actor]
		if !ok {
			return ErrMemberNotFound
		}
		need := PermKickMembers
		if ban {
			need = PermBanMembers
		}
		if !HasPermission(a.Role, need) {
			return ErrInsufficientPermissions
		}
		if !CanAdminister(a.Role, t.Role) {
			return ErrInsufficientRank
		}
	} else if t.Role.Rank >= RoleOwner.Rank {
		return ErrCannotTargetOwner // the owner cannot walk away from their own group
	}
	ban = ban && actor != target
	var newEpochKey *GroupEpochKey
	if ban {
		// evict the target from the epoch key as BanMember does, before the ban takes effect
		if newEpochKey, err = m.keyEngine.RotateKey(groupID); err != nil {
			return fmt.Errorf("cryptographic key rotation failed: %w", err)
		}
	}
	delete(st.Members, target)
	if ban {
		st.Bans[target] = "banned"
	}
	m.emit("groups:member_removed", map[string]any{"group_id": groupID, "member": target.String(), "ban": ban})
	if newEpochKey != nil {
		m.emit("groups:epoch_rotated", map[string]any{"group_id": groupID, "epoch": newEpochKey.Epoch})
	}
	return nil
}

// AddMemberAs adds a member on the word of actor, who needs the permission to manage roles. The
// peer ID has to be the hash of the key.
func (m *GroupManager) AddMemberAs(groupID string, actor types.PeerID, member types.PeerID, pub types.IdentityPublicKey) (err error) {
	defer func() { m.logOp("remote add", groupID, err, "actor", shortPeer(actor), "member", shortPeer(member)) }()
	if IDOfKey(pub) != member {
		return ErrInvalidSignature
	}
	st, err := m.state(groupID)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	a, ok := st.Members[actor]
	if !ok {
		return ErrMemberNotFound
	}
	if !HasPermission(a.Role, PermManageRoles) {
		return ErrInsufficientPermissions
	}
	return addLocked(st, member, pub)
}

func addLocked(st *GroupState, member types.PeerID, pub types.IdentityPublicKey) error {
	if _, banned := st.Bans[member]; banned {
		return ErrMemberAlreadyBanned
	}
	if _, ok := st.Members[member]; ok {
		return nil
	}
	role := RoleMember
	if st.Genesis.Type == GroupTypePublicChannel {
		role = RoleChannelSubscriber
	}
	st.Members[member] = MemberProfile{PeerID: member, PubKey: pub, Role: role, JoinedAt: time.Now().UnixMilli()}
	return nil
}

// SelfJoin adds a member who announces themselves with the invite in hand.
func (m *GroupManager) SelfJoin(groupID string, member types.PeerID, pub types.IdentityPublicKey) error {
	if IDOfKey(pub) != member {
		m.logOp("self join", groupID, ErrInvalidSignature, "member", shortPeer(member))
		return ErrInvalidSignature
	}
	return m.JoinGroup(groupID, member, pub) // JoinGroup logs the outcome
}

// MergeMembers takes over a member list. By default it only adds the members that are not known
// yet and changes nothing already known (roles move through ApplyRole). authoritative is for a list
// from the owner (or from an invite): every role in it is taken, an owner included, so that a change
// of ownership reaches newcomers and nodes that were offline.
func (m *GroupManager) MergeMembers(groupID string, list []MemberProfile, authoritative bool) error {
	st, err := m.state(groupID)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	added, badKey, banned := 0, 0, 0
	defer func() {
		args := []any{"entries", len(list), "added", added, "authoritative", authoritative}
		if badKey > 0 || banned > 0 {
			// entries whose ID is not the hash of their key are forged or corrupt
			m.logger().Warn("Skipped entries of a group member list", "group_id", groupID, "bad_key", badKey, "banned", banned, "entries", len(list))
		}
		m.logger().Debug("Merged group member list", append([]any{"group_id", groupID}, args...)...)
	}()
	for _, p := range list {
		if IDOfKey(p.PubKey) != p.PeerID {
			badKey++
			continue
		}
		role, rerr := m.resolveRole(groupID, p.Role.Name)
		if rerr != nil {
			// a custom role whose definition has not reached us yet: a member's rights under its
			// name until it does (RefreshRoles)
			m.logger().Debug("Unknown role in group member list, taking it as member", "group_id", groupID, "member", shortPeer(p.PeerID), "role", p.Role.Name)
			role = RoleMember
			if validRoleName(p.Role.Name) {
				role.Name = p.Role.Name
			}
		}
		prof, known := st.Members[p.PeerID]
		if !known {
			if err := addLocked(st, p.PeerID, p.PubKey); err != nil {
				banned++
				continue
			}
			added++
			prof = st.Members[p.PeerID]
			if !authoritative && role.Rank >= RoleAdmin.Rank {
				continue // ranks that only the owner can give
			}
			if authoritative || role.Rank < RoleAdmin.Rank {
				prof.Role = role
				st.Members[p.PeerID] = prof
			}
		} else if authoritative {
			prof.Role = role
			st.Members[p.PeerID] = prof
		}
		if authoritative && role.Rank >= RoleOwner.Rank {
			demoteOtherOwners(st, p.PeerID)
		}
	}
	return nil
}

// demoteOtherOwners keeps one owner: everybody else who held the rank becomes an admin.
func demoteOtherOwners(st *GroupState, owner types.PeerID) {
	for id, prof := range st.Members {
		if id != owner && prof.Role.Rank >= RoleOwner.Rank {
			prof.Role = RoleAdmin
			st.Members[id] = prof
		}
	}
}

// ApplyTransfer hands the group to target on the word of actor, who has to be its owner; the former
// owner becomes an admin.
func (m *GroupManager) ApplyTransfer(groupID string, actor, target types.PeerID) (err error) {
	defer func() {
		m.logOp("remote transfer", groupID, err, "actor", shortPeer(actor), "target", shortPeer(target))
	}()
	st, err := m.state(groupID)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	a, ok := st.Members[actor]
	if !ok {
		return ErrMemberNotFound
	}
	if a.Role.Rank < RoleOwner.Rank {
		return ErrInsufficientPermissions
	}
	t, ok := st.Members[target]
	if !ok {
		return ErrMemberNotFound
	}
	if actor == target {
		return nil
	}
	t.Role = RoleOwner
	st.Members[target] = t
	a.Role = RoleAdmin
	st.Members[actor] = a
	m.emit("groups:owner_changed", map[string]any{"group_id": groupID, "owner": target.String()})
	return nil
}

// IsOwner says whether peer is the owner of the group in this node's view.
func (m *GroupManager) IsOwner(groupID string, peer types.PeerID) bool {
	r, err := m.GetMemberRole(groupID, peer)
	return err == nil && r.Rank >= RoleOwner.Rank
}

// DropGroup forgets a group (the node was removed from it or left).
func (m *GroupManager) DropGroup(groupID string) {
	m.mu.Lock()
	_, known := m.groups[groupID]
	delete(m.groups, groupID)
	m.mu.Unlock()
	if known {
		m.logOp("drop", groupID, nil)
	}
}
