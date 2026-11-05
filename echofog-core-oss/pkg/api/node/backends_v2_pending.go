package node

import (
	"context"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/contacts"
	"github.com/echofog/echofog-core/pkg/dto"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// v2PendingBackend stands in for the v2 domain backends until T11.2-T11.4 build them.
// Every method answers ErrV2NotBuilt; a method whose signature has no error result returns
// the zero value, because a caller of that shape cannot be told. See BACKENDS.md.

type v2PendingBackend struct{}

func (v2PendingBackend) AddBootstrapNode(entry string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) AddRelay(link string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) CheckProxy(addr string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ConnectToPeer(addr string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) CreatePass(r PassRequest) (PassView, error) {
	return PassView{}, ErrV2NotBuilt
}

func (v2PendingBackend) DeletePass(id string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ExtendPass(id string, r PassRequest) (PassView, error) {
	return PassView{}, ErrV2NotBuilt
}

func (v2PendingBackend) ForgetInvite(peerID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) GetAccessStatus() (AccessStatus, error) {
	return AccessStatus{}, ErrV2NotBuilt
}

func (v2PendingBackend) GetAnonymity() (AnonymityStatus, error) {
	return AnonymityStatus{}, ErrV2NotBuilt
}

func (v2PendingBackend) GetBootstrapNodes() (BootstrapStatus, error) {
	return BootstrapStatus{}, ErrV2NotBuilt
}

func (v2PendingBackend) GetGuardStatus() (GuardStatus, error) {
	return GuardStatus{}, ErrV2NotBuilt
}

func (v2PendingBackend) GetNetworkStats() NetworkStats {
	return NetworkStats{}
}

func (v2PendingBackend) GetPass(id string) (PassView, error) {
	return PassView{}, ErrV2NotBuilt
}

func (v2PendingBackend) GetPeers() []dto.PeerDTO {
	return nil
}

func (v2PendingBackend) GetProxySettings() (ProxyStatus, error) {
	return ProxyStatus{}, ErrV2NotBuilt
}

func (v2PendingBackend) GetRelaySettings() (RelaySettingsDTO, error) {
	return RelaySettingsDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) GetRelayStatus() (RelayStatus, error) {
	return RelayStatus{}, ErrV2NotBuilt
}

func (v2PendingBackend) JoinInvite(link string) (string, error) {
	return "", ErrV2NotBuilt
}

func (v2PendingBackend) ListPasses() ([]PassView, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) MeasureUploadCapacity() (int, error) {
	return 0, ErrV2NotBuilt
}

func (v2PendingBackend) OnPeerState(cb func(PeerStateDTO)) func() {
	return nil
}

func (v2PendingBackend) ParseInvite(link string) (InviteInfo, error) {
	return InviteInfo{}, ErrV2NotBuilt
}

func (v2PendingBackend) PeerDiscoveryHandler() <-chan dto.PeerDTO {
	return closedChannel[dto.PeerDTO]()
}

func (v2PendingBackend) PinGuard(entry string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) RemoveBootstrapNode(entry string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) RemoveJoinedPeer(peerID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) RemovePassDevice(id, peerID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) RevokePass(id string) (PassView, error) {
	return PassView{}, ErrV2NotBuilt
}

func (v2PendingBackend) RotateAccessSecret() (string, error) {
	return "", ErrV2NotBuilt
}

func (v2PendingBackend) SetAccessMode(mode string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetAnonymity(mode string, walletDirect bool) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetGuardMode(mode string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetProxySettings(proxy, callRoute string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetRelaySettings(r RelaySettingsDTO) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) StartLocalDiscovery(myAlias string) {
}

func (v2PendingBackend) StartVPNVia(target string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) StopLocalDiscovery() {
}

func (v2PendingBackend) UnpinGuard(entry string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) AcceptMessageRequest(peerID, alias string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) BanContact(peerID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) BlockPeer(peerID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) CanRing(peerID string) bool {
	return false
}

func (v2PendingBackend) DeleteContact(peerID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) DismissMessageRequest(peerID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) GetBlockedPeers() ([]contacts.Blocked, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) GetContacts() ([]dto.ContactDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) GetMessageRequests() ([]contacts.Request, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) NewContact(contact dto.ContactDTO) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetContactProfile(peerID, alias, avatarBase64 string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) UnblockPeer(peerID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatAttachments(ref ChatRef) (ChatAttachments, error) {
	return ChatAttachments{}, ErrV2NotBuilt
}

func (v2PendingBackend) ChatDelete(ref ChatRef, id string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatEdit(ref ChatRef, id, text string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatHistory(ref ChatRef, cursor string, limit int) (ChatPage, error) {
	return ChatPage{}, ErrV2NotBuilt
}

func (v2PendingBackend) ChatMarkRead(ref ChatRef, id string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatMessage(ref ChatRef, id string) (ChatMessage, error) {
	return ChatMessage{}, ErrV2NotBuilt
}

func (v2PendingBackend) ChatPin(ref ChatRef, id string, on bool) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatPins(ref ChatRef) ([]ChatMessage, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) ChatReact(ref ChatRef, id, emoji string, on bool) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatRows() ([]ConversationDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) ChatSend(ref ChatRef, req SendRequest) (ChatMessage, error) {
	return ChatMessage{}, ErrV2NotBuilt
}

func (v2PendingBackend) ChatThread(ref ChatRef, root, cursor string, limit int) (ChatPage, error) {
	return ChatPage{}, ErrV2NotBuilt
}

func (v2PendingBackend) Conversations() ([]ConversationDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) GetMessages(contactID string, limit int, beforeTimestamp uint64) ([]dto.MessageDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) ListSavedMessages() ([]dto.SavedMessageDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) MessageHandler() <-chan *dto.MessageDTO {
	return closedChannel[*dto.MessageDTO]()
}

func (v2PendingBackend) OnMessageState(cb func(MessageStateDTO)) {
}

func (v2PendingBackend) ResetEncryptionSession(peerID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SaveMessage(m dto.SavedMessageDTO) (dto.SavedMessageDTO, error) {
	return dto.SavedMessageDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) SendMessage(pubKeyHex string, text string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SubscribeConversations(cb func(ConvChange)) {
}

func (v2PendingBackend) UnsaveMessage(id string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) AddGroupMember(groupID, pubKeyHex string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatCategoriesSetup(ref ChatRef, textName, voiceName string) ([]string, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) ChatChannelMove(ref ChatRef, id string, index int) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatChannelPlace(ref ChatRef, id, parent string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatChannelRemove(ref ChatRef, id string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatChannelSave(ref ChatRef, spec ChannelSpec) (ChannelDTO, error) {
	return ChannelDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) ChatChannels(ref ChatRef) ([]ChannelDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) ChatGroupPhoto(ref ChatRef, photoBase64 string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatMainChannel(ref ChatRef) (ChannelDTO, error) {
	return ChannelDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) ChatMainChannelSave(ref ChatRef, spec ChannelSpec) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatThreadTitle(ref ChatRef, root, title string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ChatThreads(ref ChatRef) ([]ChatMessage, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) CreateGroup(title, description, kind string) (GroupDTO, error) {
	return GroupDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) DeleteGroup(groupID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) GetGroupInvite(groupID string) (string, error) {
	return "", ErrV2NotBuilt
}

func (v2PendingBackend) GetGroupMessages(groupID string, limit int) ([]GroupMessageDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) GroupInviteContacts(groupID string, peerIDs []string, role string) (int, error) {
	return 0, ErrV2NotBuilt
}

func (v2PendingBackend) GroupInviteCreate(groupID string, spec GroupInviteSpec) (GroupInviteDTO, error) {
	return GroupInviteDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) GroupInviteRevoke(groupID, nonce string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) GroupInvites(groupID string) ([]GroupInviteDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) GroupJoinDecide(groupID, peerID string, accept bool) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) GroupJoinRequests(groupID string) ([]GroupJoinRequestDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) GroupModerationLog(groupID string) ([]GroupModEntry, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) GroupRoleRemove(groupID, roleID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) GroupRoleSave(groupID string, r GroupRoleDTO) (GroupRoleDTO, error) {
	return GroupRoleDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) GroupRoles(groupID string) ([]GroupRoleDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) GroupSnapshotExport(groupID, path, password string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) GroupSnapshotImport(path, password string) (GroupSnapshotInfo, error) {
	return GroupSnapshotInfo{}, ErrV2NotBuilt
}

func (v2PendingBackend) GroupTimeout(groupID, memberID string, minutes int) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) InspectGroupInvite(text string) (GroupInvitePreview, error) {
	return GroupInvitePreview{}, ErrV2NotBuilt
}

func (v2PendingBackend) JoinGroup(invite string) (GroupDTO, error) {
	return GroupDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) LeaveGroup(groupID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ListGroupMembers(groupID string) ([]GroupMemberDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) ListGroups() ([]GroupDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) RemoveGroupMember(groupID, memberID string, ban bool) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SendGroupMessage(groupID, text string) (GroupMessageDTO, error) {
	return GroupMessageDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) SetGroupRole(groupID, memberID, role string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SubscribeGroupEvents(cb func(GroupEvent)) {
}

func (v2PendingBackend) TransferGroupOwnership(groupID, memberID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) AcceptFile(id string) (dto.FileTransferDTO, error) {
	return dto.FileTransferDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) CancelFile(id string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) DeclineFile(id string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) DownloadDir() string {
	return ""
}

func (v2PendingBackend) FileTransferPath(id string) (string, error) {
	return "", ErrV2NotBuilt
}

func (v2PendingBackend) GetFileTransfer(id string) (dto.FileTransferDTO, error) {
	return dto.FileTransferDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) ListFileTransfers(peerIDHex string) ([]dto.FileTransferDTO, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) OnFileTransfer(cb func(dto.FileTransferDTO)) func() {
	return nil
}

func (v2PendingBackend) SendFile(pubKeyHex string, path string) (dto.FileTransferDTO, error) {
	return dto.FileTransferDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) SendGroupFile(groupID, channel, thread, path string) (GroupMessageDTO, error) {
	return GroupMessageDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) ActiveGroupCall(groupID string) (GroupCall, bool) {
	return GroupCall{}, false
}

func (v2PendingBackend) CallStreamStats(streamID string) (StreamStatsDTO, error) {
	return StreamStatsDTO{}, ErrV2NotBuilt
}

func (v2PendingBackend) CaptureRTPBudget(streamID string) int {
	return 0
}

func (v2PendingBackend) EndGroupCall(groupID, callID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) EndRoomCall(groupID, room, callID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) GetCallManager() *call.CallManager {
	return nil
}

func (v2PendingBackend) GetCallPath() CallPath {
	return CallPath{}
}

func (v2PendingBackend) GetCallRelays() []CallRelay {
	return nil
}

func (v2PendingBackend) GetCallRoster() dto.CallRosterDTO {
	return dto.CallRosterDTO{}
}

func (v2PendingBackend) GetCallSlots() []dto.CallMediaSlotDTO {
	return nil
}

func (v2PendingBackend) InviteToCall(pubKeyHex string) (CallInvite, error) {
	return CallInvite{}, ErrV2NotBuilt
}

func (v2PendingBackend) JoinCall(callID string, coordPeerID string) (string, error) {
	return "", ErrV2NotBuilt
}

func (v2PendingBackend) KeepGroupCallAlive(groupID, callID, coordinator string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) KeepRoomCallAlive(groupID, room, callID, coordinator string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) LeaveCall() error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) LeaveRoomCall(groupID, room, callID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ListStreamQualities() []dto.StreamQualityDTO {
	return nil
}

func (v2PendingBackend) PublishCaptureRTP(role string, packet []byte) {
}

func (v2PendingBackend) ReportCallMember(peerHex, reason string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) RequestRelayChange(streamID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetCallBackdrop(name string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetCaptureEncoder(role string, enc CaptureEncoder) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetDisplayRate(hz int) {
}

func (v2PendingBackend) SetUploadCapacity(kbps int) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetVADState(speaking bool, audioDb float64) {
}

func (v2PendingBackend) SetupCallCallbacks(
	onActiveSpeaker func(speaker string, speaking bool, db float32),
	onStreamAnnounced func(streamID string, broadcaster string, mediaType string, bitrate int),
	onStreamClosed func(streamID string),
	onTopologyUpdated func(epoch uint64, coordinator string),
) {
}

func (v2PendingBackend) SetupCallMediaCallbacks(onSlot func(dto.CallMediaSlotDTO), onKicked func(reason string)) {
}

func (v2PendingBackend) SetupStreamInfoCallback(fn func(info dto.StreamInfoDTO)) {
}

func (v2PendingBackend) StartGroupCall(groupID string) (GroupCall, error) {
	return GroupCall{}, ErrV2NotBuilt
}

func (v2PendingBackend) StartRoomCall(groupID, room string) (GroupCall, error) {
	return GroupCall{}, ErrV2NotBuilt
}

func (v2PendingBackend) StartScreenShare(streamID string, bitrate int) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) StartStream(streamID, mediaType, qualityID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) StopScreenShare(streamID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SubmitCallAnswer(sdpAnswer string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SwitchCallRelay(via string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) UnwatchStream(streamID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) UpdateStreamQuality(streamID, qualityID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) WatchStream(streamID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) CheckSites(exitPeerID string, sites []string) ([]coreNode.SiteCheck, error) {
	return nil, ErrV2NotBuilt
}

func (v2PendingBackend) ClearVPNHistory() error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ConnectLastExit() error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) ForgetExit(peer string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) GetExitFreePeers() []string {
	return nil
}

func (v2PendingBackend) GetExitGatewayStatus() ExitGatewayStatus {
	return ExitGatewayStatus{}
}

func (v2PendingBackend) GetExitPayment() ExitPaymentStatus {
	return ExitPaymentStatus{}
}

func (v2PendingBackend) GetProbeDomains() []string {
	return nil
}

func (v2PendingBackend) GetRelayMarket() []RelayInfo {
	return nil
}

func (v2PendingBackend) GetSavedExits() []SavedExit {
	return nil
}

func (v2PendingBackend) GetSharingStatus() SharingStatus {
	return SharingStatus{}
}

func (v2PendingBackend) GetSiteLists() (SiteLists, error) {
	return SiteLists{}, ErrV2NotBuilt
}

func (v2PendingBackend) GetVPNAutoSwitch() bool {
	return false
}

func (v2PendingBackend) GetVPNHistory() []VPNSession {
	return nil
}

func (v2PendingBackend) GetVPNStatus() (st VPNStatus) {
	return VPNStatus{}
}

func (v2PendingBackend) KillSwitchOn() bool {
	return false
}

func (v2PendingBackend) ListExitNodes() []ExitNode {
	return nil
}

func (v2PendingBackend) PayExit(ctx context.Context, amountUSDT string) (string, error) {
	return "", ErrV2NotBuilt
}

func (v2PendingBackend) ProbeExitNode(peerID string) (ProbeReport, error) {
	return ProbeReport{}, ErrV2NotBuilt
}

func (v2PendingBackend) RefreshExitNodes() (int, error) {
	return 0, ErrV2NotBuilt
}

func (v2PendingBackend) SetAutoPay(enabled bool, chunkUSDT, dailyCapUSDT string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetCustomSites(sites []string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetExitFavorite(peer string, on bool) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetExitFreePeers(ids []string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetExitGatewayEnabled(on bool) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetExitName(peer, name string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetKillSwitch(on bool) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetPayAsset(asset string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetProbeDomains(domains []string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetVPNAutoSwitch(on bool) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) SetWholeComputer(on bool) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) StartSharing(cfg SharingConfig) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) StartVPN(relayPeerID string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) StartVPNAt(relayPeerID, listen string) error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) StopSharing() error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) StopVPN() error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) StopVPNForSwitch() error {
	return ErrV2NotBuilt
}

func (v2PendingBackend) WholeComputerSupported() bool {
	return false
}
