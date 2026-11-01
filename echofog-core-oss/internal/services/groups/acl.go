package groups

import (
	"crypto/ed25519"
	"errors"
)

var (
	ErrInsufficientPermissions = errors.New("insufficient permissions for this action")
	ErrInsufficientRank        = errors.New("insufficient rank: actor cannot moderate equal or higher rank")
	ErrCannotTargetOwner       = errors.New("cannot moderate or demote group owner")
	ErrInvalidSignature        = errors.New("cryptographic signature verification failed")
	ErrMemberNotFound          = errors.New("member not found in group")
	ErrMemberAlreadyBanned     = errors.New("member is already banned")
	ErrMemberNotBanned         = errors.New("member is not banned")
	ErrUnknownRole             = errors.New("unknown role requested")
)

// HasPermission checks if the given role holds the requested permission bits.
func HasPermission(role Role, perm PermissionMask) bool {
	return (role.Permissions & perm) == perm
}

// CanAdminister returns true only if actor strictly outranks target.
// An admin cannot ban another admin; nobody can ban the owner.
func CanAdminister(actorRole Role, targetRole Role) bool {
	if targetRole.Rank >= RoleOwner.Rank {
		return false // Owner is untouchable
	}
	return actorRole.Rank > targetRole.Rank
}

// CanAssignRole returns true only if actor strictly outranks the role being assigned.
// An admin (rank 50) cannot create another admin (rank 50) or owner (rank 100).
func CanAssignRole(actorRole Role, newRole Role) bool {
	return actorRole.Rank > newRole.Rank
}

// ResolveRole parses standard role names into Role structs.
func ResolveRole(name string) (Role, error) {
	switch name {
	case "owner":
		return RoleOwner, nil
	case "admin":
		return RoleAdmin, nil
	case "moderator":
		return RoleModerator, nil
	case "member":
		return RoleMember, nil
	case "subscriber":
		return RoleChannelSubscriber, nil
	case "restricted":
		return RoleRestricted, nil
	default:
		return Role{}, ErrUnknownRole
	}
}

// VerifyGenesisSignature checks Ed25519 signature of the group genesis block.
func VerifyGenesisSignature(g *GroupGenesisManifest) bool {
	digest := g.CanonicalHash()
	pubKey := ed25519.PublicKey(g.CreatorPub[:])
	return ed25519.Verify(pubKey, digest, g.Signature[:])
}

// VerifyActionSignature checks Ed25519 signature of an administrative action.
func VerifyActionSignature(a *GroupAction) bool {
	digest := a.CanonicalHash()
	pubKey := ed25519.PublicKey(a.ActorPub[:])
	return ed25519.Verify(pubKey, digest, a.Signature[:])
}

// VerifyTombstoneSignature checks Ed25519 signature of a message tombstone.
func VerifyTombstoneSignature(t *MessageTombstone) bool {
	digest := t.CanonicalHash()
	pubKey := ed25519.PublicKey(t.ActorPub[:])
	return ed25519.Verify(pubKey, digest, t.Signature[:])
}

// validRoleName: a role's name as a group's log may define it ([a-z0-9-], at most 32).
func validRoleName(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
