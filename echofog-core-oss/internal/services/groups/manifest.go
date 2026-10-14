package groups

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/echofog/echofog-core/pkg/api/types"
)

type GroupType byte

const (
	GroupTypePrivateGroup  GroupType = 1 // Closed encrypted group (invite/approval only)
	GroupTypePublicChannel GroupType = 2 // Broadcast channel (one/few post, everyone reads)
	GroupTypeOpenSwarm     GroupType = 3 // Open community room (anyone can join and chat)
)

type PermissionMask uint32

const (
	PermSendMessages   PermissionMask = 1 << 0 // Write text messages
	PermSendMedia      PermissionMask = 1 << 1 // Send files/photos/audio
	PermJoinVoice      PermissionMask = 1 << 2 // Speak in voice backbone tree
	PermPublishStream  PermissionMask = 1 << 3 // Broadcast screen/webcam stream
	PermDeleteMessages PermissionMask = 1 << 4 // Delete other users' messages (Tombstone)
	PermKickMembers    PermissionMask = 1 << 5 // Kick member temporarily
	PermBanMembers     PermissionMask = 1 << 6 // Permanent ban + cryptographic key eviction
	PermManageRoles    PermissionMask = 1 << 7 // Promote/demote members of lower rank
	PermManageGroup    PermissionMask = 1 << 8 // Modify name/avatar/description
)

// Role defines hierarchy rank and permission bitmask.
type Role struct {
	Name        string         `json:"name"`
	Rank        int            `json:"rank"` // 100=Owner, 50=Admin, 25=Moderator, 10=Member, 1=Restricted
	Permissions PermissionMask `json:"permissions"`
}

var (
	RoleOwner = Role{
		Name: "owner",
		Rank: 100,
		Permissions: PermSendMessages | PermSendMedia | PermJoinVoice | PermPublishStream |
			PermDeleteMessages | PermKickMembers | PermBanMembers | PermManageRoles | PermManageGroup,
	}

	RoleAdmin = Role{
		Name: "admin",
		Rank: 50,
		Permissions: PermSendMessages | PermSendMedia | PermJoinVoice | PermPublishStream |
			PermDeleteMessages | PermKickMembers | PermBanMembers | PermManageRoles,
	}

	RoleModerator = Role{
		Name:        "moderator",
		Rank:        25,
		Permissions: PermSendMessages | PermSendMedia | PermJoinVoice | PermDeleteMessages | PermKickMembers,
	}

	RoleMember = Role{
		Name:        "member",
		Rank:        10,
		Permissions: PermSendMessages | PermSendMedia | PermJoinVoice,
	}

	RoleChannelSubscriber = Role{
		Name:        "subscriber",
		Rank:        10,
		Permissions: 0, // Read-only by default in channels
	}

	RoleRestricted = Role{
		Name:        "restricted",
		Rank:        1,
		Permissions: 0, // Muted / Read-only
	}
)

// GroupGenesisManifest is the signed immutable root of a group or channel.
type GroupGenesisManifest struct {
	GroupID     string                  `json:"group_id"`
	Title       string                  `json:"title"`
	Description string                  `json:"description"`
	Type        GroupType               `json:"type"`
	Creator     types.PeerID            `json:"creator"`
	CreatorPub  types.IdentityPublicKey `json:"creator_pub"`
	CreatedAt   int64                   `json:"created_at"`
	Salt        [16]byte                `json:"salt"`
	Signature   types.Signature         `json:"signature"`
}

// CanonicalHash generates the cryptographic digest to verify the creator's signature.
func (g *GroupGenesisManifest) CanonicalHash() []byte {
	h := sha256.New()
	h.Write([]byte(g.GroupID))
	h.Write([]byte(g.Title))
	h.Write([]byte{byte(g.Type)})
	h.Write(g.Creator[:])
	h.Write(g.CreatorPub[:])

	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(g.CreatedAt))
	h.Write(tsBuf[:])
	h.Write(g.Salt[:])

	return h.Sum(nil)
}

// ActionType defines the administrative operation.
type ActionType string

const (
	ActionTypePromote   ActionType = "promote"
	ActionTypeDemote    ActionType = "demote"
	ActionTypeBan       ActionType = "ban"
	ActionTypeUnban     ActionType = "unban"
	ActionTypeKick      ActionType = "kick"
	ActionTypeTombstone ActionType = "tombstone"
	ActionTypeRotateKey ActionType = "rotate_key"
)

// GroupAction represents a signed administrative action in the group's Action DAG.
type GroupAction struct {
	ActionID  string                  `json:"action_id"`
	GroupID   string                  `json:"group_id"`
	Type      ActionType              `json:"type"`
	Actor     types.PeerID            `json:"actor"`
	ActorPub  types.IdentityPublicKey `json:"actor_pub"`
	Target    types.PeerID            `json:"target,omitempty"`
	NewRole   string                  `json:"new_role,omitempty"`
	Reason    string                  `json:"reason,omitempty"`
	Timestamp int64                   `json:"timestamp"`
	Signature types.Signature         `json:"signature"`
}

// CanonicalHash calculates the signing digest for a group action.
func (a *GroupAction) CanonicalHash() []byte {
	h := sha256.New()
	h.Write([]byte(a.ActionID))
	h.Write([]byte(a.GroupID))
	h.Write([]byte(a.Type))
	h.Write(a.Actor[:])
	h.Write(a.ActorPub[:])
	h.Write(a.Target[:])
	h.Write([]byte(a.NewRole))
	h.Write([]byte(a.Reason))

	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(a.Timestamp))
	h.Write(tsBuf[:])

	return h.Sum(nil)
}

// MessageTombstone is a signed proof that a message has been permanently deleted.
type MessageTombstone struct {
	MessageID [16]byte                `json:"message_id"`
	GroupID   string                  `json:"group_id"`
	DeletedBy types.PeerID            `json:"deleted_by"`
	ActorPub  types.IdentityPublicKey `json:"actor_pub"`
	DeletedAt int64                   `json:"deleted_at"`
	Signature types.Signature         `json:"signature"`
}

// CanonicalHash calculates the signing digest for a message tombstone.
func (t *MessageTombstone) CanonicalHash() []byte {
	h := sha256.New()
	h.Write(t.MessageID[:])
	h.Write([]byte(t.GroupID))
	h.Write(t.DeletedBy[:])
	h.Write(t.ActorPub[:])

	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(t.DeletedAt))
	h.Write(tsBuf[:])

	return h.Sum(nil)
}

// MemberProfile stores current member state in BadgerDB.
type MemberProfile struct {
	PeerID    types.PeerID            `json:"peer_id"`
	PubKey    types.IdentityPublicKey `json:"pub_key"`
	Role      Role                    `json:"role"`
	JoinedAt  int64                   `json:"joined_at"`
	IsBanned  bool                    `json:"is_banned"`
	BanReason string                  `json:"ban_reason,omitempty"`
}
