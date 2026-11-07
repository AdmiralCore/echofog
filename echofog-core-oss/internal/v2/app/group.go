package app

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
)

var errNotBuilt = errors.New("app: not built")

// GroupInfo is one line of the group list.
type GroupInfo struct {
	ID, Title, Description, Kind string
	Role                         string
	Owner                        bool
	Members                      int
	Created                      int64
	Pending                      bool // asked to join, waiting for a decision
	Unread                       int
}

// GroupMember is one member of a group.
type GroupMember struct {
	Node, Role, Title string
	Rank              int
	Owner, Self       bool
	Alias             string
	Online            bool
	Timeout           int64 // unix seconds; 0: none
}

// GroupRole is a role of a group.
type GroupRole struct {
	ID, Title string
	Color     uint32
	Rank      int
	Perms     uint32
	Builtin   bool
}

// InviteSpec is an invitation as its maker wants it.
type InviteSpec struct {
	TTL         time.Duration
	Uses        uint32
	Role, Label string
	Approval    bool
}

// GroupInvite is an invitation of a group.
type GroupInvite struct {
	Nonce, Role, Label, Issuer string
	Exp                        int64
	Uses, Used                 uint32
	Revoked, Approval          bool
	Link                       string // only for one made on this node
}

// InvitePreview is what a link shows before joining.
type InvitePreview struct {
	GroupID, Title, Description, Kind string
	Expires                           int64
	Approval                          bool
	Member                            bool // already in
}

// JoinRequest is a newcomer waiting for a decision.
type JoinRequest struct {
	Node   string
	TS     int64
	Invite string
}

func (a *App) CreateGroup(ctx context.Context, title, desc, kind string) (GroupInfo, error) {
	return GroupInfo{}, errNotBuilt
}
func (a *App) Groups() ([]GroupInfo, error)                    { return nil, errNotBuilt }
func (a *App) GroupInfoOf(id string) (GroupInfo, error)        { return GroupInfo{}, errNotBuilt }
func (a *App) LeaveGroup(ctx context.Context, id string) error { return errNotBuilt }
func (a *App) ForgetGroup(id string) error                     { return errNotBuilt }
func (a *App) GroupMembers(id string) ([]GroupMember, error)   { return nil, errNotBuilt }
func (a *App) GroupSetRole(ctx context.Context, id, node, role string) error {
	return errNotBuilt
}
func (a *App) GroupTransfer(ctx context.Context, id, node string) error { return errNotBuilt }
func (a *App) GroupRemove(ctx context.Context, id, node string, ban bool) error {
	return errNotBuilt
}
func (a *App) GroupTimeout(ctx context.Context, id, node string, minutes int) error {
	return errNotBuilt
}
func (a *App) GroupRoles(id string) ([]GroupRole, error) { return nil, errNotBuilt }
func (a *App) GroupRoleSave(ctx context.Context, id string, r GroupRole) (GroupRole, error) {
	return GroupRole{}, errNotBuilt
}
func (a *App) GroupRoleRemove(ctx context.Context, id, role string) error { return errNotBuilt }
func (a *App) GroupChannels(id string) ([]convlog.Channel, error)         { return nil, errNotBuilt }
func (a *App) GroupChannelSave(ctx context.Context, id string, s convlog.ChannelSpec) (convlog.Channel, error) {
	return convlog.Channel{}, errNotBuilt
}
func (a *App) GroupChannelRemove(ctx context.Context, id, ch string) error { return errNotBuilt }
func (a *App) GroupChannelMove(ctx context.Context, id, ch string, index int) error {
	return errNotBuilt
}
func (a *App) GroupChannelPlace(ctx context.Context, id, ch, parent string) error {
	return errNotBuilt
}
func (a *App) GroupCategoriesSetup(ctx context.Context, id, text, voice string) ([]string, error) {
	return nil, errNotBuilt
}
func (a *App) GroupMainChannel(id string) (convlog.Channel, error) {
	return convlog.Channel{}, errNotBuilt
}
func (a *App) GroupMainChannelSave(ctx context.Context, id string, s convlog.ChannelSpec) error {
	return errNotBuilt
}
func (a *App) GroupPhoto(ctx context.Context, id string, photo []byte) error { return errNotBuilt }
func (a *App) GroupInviteCreate(ctx context.Context, id string, s InviteSpec) (GroupInvite, error) {
	return GroupInvite{}, errNotBuilt
}
func (a *App) GroupInvites(id string) ([]GroupInvite, error) { return nil, errNotBuilt }
func (a *App) GroupInviteRevoke(ctx context.Context, id, nonce string) error {
	return errNotBuilt
}
func (a *App) GroupInviteContacts(ctx context.Context, id string, nodes []string, role string) (int, error) {
	return 0, errNotBuilt
}
func (a *App) InspectGroupInvite(link string) (InvitePreview, error) {
	return InvitePreview{}, errNotBuilt
}
func (a *App) JoinGroup(ctx context.Context, link string) (GroupInfo, error) {
	return GroupInfo{}, errNotBuilt
}
func (a *App) GroupJoinRequests(id string) ([]JoinRequest, error) { return nil, errNotBuilt }
func (a *App) GroupJoinDecide(ctx context.Context, id, node string, accept bool) error {
	return errNotBuilt
}
func (a *App) GroupModerationLog(id string) ([]convlog.ModEntry, error) { return nil, errNotBuilt }
func (a *App) ChatThreads(ctx context.Context, ref ChatRef) ([]convlog.Message, error) {
	return nil, errNotBuilt
}
func (a *App) ChatThreadTitle(ctx context.Context, ref ChatRef, root, title string) error {
	return errNotBuilt
}
func (a *App) GroupRights(id, channel string) (convlog.Rights, error) {
	return convlog.Rights{}, errNotBuilt
}

// OnGroupMessage takes a join message of chat/2; it reports whether it was one.
func (a *App) OnGroupMessage(ctx context.Context, from [32]byte, pm *chatpb.PersonalMessageProto) bool {
	return false
}
