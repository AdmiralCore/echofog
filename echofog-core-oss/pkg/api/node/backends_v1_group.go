package node

import "github.com/echofog/echofog-core/internal/config"

// v1GroupBackend is the protocol 1 implementation of groupBackend: the code that used to sit in
// the NodeApi methods of this domain, moved here unchanged by T11.1c. The NodeApi methods below
// are one-line delegates.
type v1GroupBackend struct{ *NodeApi }

// groupDomain is the groups backend of this facade, chosen the same way as netDomain.
func (n *NodeApi) groupDomain() groupBackend {
	if n.backends.group != nil {
		return n.backends.group
	}
	if n.node != nil && n.node.Cfg != nil && n.node.Cfg.ProtocolOrDefault() == config.ProtocolV2 {
		return v2PendingBackend{}
	}
	return &v1GroupBackend{n}
}

func (n *NodeApi) AddGroupMember(groupID, pubKeyHex string) error {
	return n.groupDomain().AddGroupMember(groupID, pubKeyHex)
}

func (n *NodeApi) ChatCategoriesSetup(ref ChatRef, textName, voiceName string) ([]string, error) {
	return n.groupDomain().ChatCategoriesSetup(ref, textName, voiceName)
}

func (n *NodeApi) ChatChannelMove(ref ChatRef, id string, index int) error {
	return n.groupDomain().ChatChannelMove(ref, id, index)
}

func (n *NodeApi) ChatChannelPlace(ref ChatRef, id, parent string) error {
	return n.groupDomain().ChatChannelPlace(ref, id, parent)
}

func (n *NodeApi) ChatChannelRemove(ref ChatRef, id string) error {
	return n.groupDomain().ChatChannelRemove(ref, id)
}

func (n *NodeApi) ChatChannelSave(ref ChatRef, spec ChannelSpec) (ChannelDTO, error) {
	return n.groupDomain().ChatChannelSave(ref, spec)
}

func (n *NodeApi) ChatChannels(ref ChatRef) ([]ChannelDTO, error) {
	return n.groupDomain().ChatChannels(ref)
}

func (n *NodeApi) ChatGroupPhoto(ref ChatRef, photoBase64 string) error {
	return n.groupDomain().ChatGroupPhoto(ref, photoBase64)
}

func (n *NodeApi) ChatMainChannel(ref ChatRef) (ChannelDTO, error) {
	return n.groupDomain().ChatMainChannel(ref)
}

func (n *NodeApi) ChatMainChannelSave(ref ChatRef, spec ChannelSpec) error {
	return n.groupDomain().ChatMainChannelSave(ref, spec)
}

func (n *NodeApi) ChatThreadTitle(ref ChatRef, root, title string) error {
	return n.groupDomain().ChatThreadTitle(ref, root, title)
}

func (n *NodeApi) ChatThreads(ref ChatRef) ([]ChatMessage, error) {
	return n.groupDomain().ChatThreads(ref)
}

func (n *NodeApi) CreateGroup(title, description, kind string) (GroupDTO, error) {
	return n.groupDomain().CreateGroup(title, description, kind)
}

func (n *NodeApi) DeleteGroup(groupID string) error {
	return n.groupDomain().DeleteGroup(groupID)
}

func (n *NodeApi) GetGroupInvite(groupID string) (string, error) {
	return n.groupDomain().GetGroupInvite(groupID)
}

func (n *NodeApi) GetGroupMessages(groupID string, limit int) ([]GroupMessageDTO, error) {
	return n.groupDomain().GetGroupMessages(groupID, limit)
}

func (n *NodeApi) GroupInviteContacts(groupID string, peerIDs []string, role string) (int, error) {
	return n.groupDomain().GroupInviteContacts(groupID, peerIDs, role)
}

func (n *NodeApi) GroupInviteCreate(groupID string, spec GroupInviteSpec) (GroupInviteDTO, error) {
	return n.groupDomain().GroupInviteCreate(groupID, spec)
}

func (n *NodeApi) GroupInviteRevoke(groupID, nonce string) error {
	return n.groupDomain().GroupInviteRevoke(groupID, nonce)
}

func (n *NodeApi) GroupInvites(groupID string) ([]GroupInviteDTO, error) {
	return n.groupDomain().GroupInvites(groupID)
}

func (n *NodeApi) GroupJoinDecide(groupID, peerID string, accept bool) error {
	return n.groupDomain().GroupJoinDecide(groupID, peerID, accept)
}

func (n *NodeApi) GroupJoinRequests(groupID string) ([]GroupJoinRequestDTO, error) {
	return n.groupDomain().GroupJoinRequests(groupID)
}

func (n *NodeApi) GroupModerationLog(groupID string) ([]GroupModEntry, error) {
	return n.groupDomain().GroupModerationLog(groupID)
}

func (n *NodeApi) GroupRoleRemove(groupID, roleID string) error {
	return n.groupDomain().GroupRoleRemove(groupID, roleID)
}

func (n *NodeApi) GroupRoleSave(groupID string, r GroupRoleDTO) (GroupRoleDTO, error) {
	return n.groupDomain().GroupRoleSave(groupID, r)
}

func (n *NodeApi) GroupRoles(groupID string) ([]GroupRoleDTO, error) {
	return n.groupDomain().GroupRoles(groupID)
}

func (n *NodeApi) GroupSnapshotExport(groupID, path, password string) error {
	return n.groupDomain().GroupSnapshotExport(groupID, path, password)
}

func (n *NodeApi) GroupSnapshotImport(path, password string) (GroupSnapshotInfo, error) {
	return n.groupDomain().GroupSnapshotImport(path, password)
}

func (n *NodeApi) GroupTimeout(groupID, memberID string, minutes int) error {
	return n.groupDomain().GroupTimeout(groupID, memberID, minutes)
}

func (n *NodeApi) InspectGroupInvite(text string) (GroupInvitePreview, error) {
	return n.groupDomain().InspectGroupInvite(text)
}

func (n *NodeApi) JoinGroup(invite string) (GroupDTO, error) {
	return n.groupDomain().JoinGroup(invite)
}

func (n *NodeApi) LeaveGroup(groupID string) error {
	return n.groupDomain().LeaveGroup(groupID)
}

func (n *NodeApi) ListGroupMembers(groupID string) ([]GroupMemberDTO, error) {
	return n.groupDomain().ListGroupMembers(groupID)
}

func (n *NodeApi) ListGroups() ([]GroupDTO, error) {
	return n.groupDomain().ListGroups()
}

func (n *NodeApi) RemoveGroupMember(groupID, memberID string, ban bool) error {
	return n.groupDomain().RemoveGroupMember(groupID, memberID, ban)
}

func (n *NodeApi) SendGroupMessage(groupID, text string) (GroupMessageDTO, error) {
	return n.groupDomain().SendGroupMessage(groupID, text)
}

func (n *NodeApi) SetGroupRole(groupID, memberID, role string) error {
	return n.groupDomain().SetGroupRole(groupID, memberID, role)
}

func (n *NodeApi) SubscribeGroupEvents(cb func(GroupEvent)) {
	n.groupDomain().SubscribeGroupEvents(cb)
}

func (n *NodeApi) TransferGroupOwnership(groupID, memberID string) error {
	return n.groupDomain().TransferGroupOwnership(groupID, memberID)
}
