package node

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/contacts"
	"github.com/echofog/echofog-core/pkg/dto"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// This file declares the domain backends the desktop facade delegates network work to.
// T11.1a adds the declarations and the choice of implementation only; the bodies move in
// T11.1b-e (v1, next to the current code) and T11.2-T11.4 (v2, in internal/v2/app).
//
// *NodeApi itself implements every v1 interface: that is the protocol 1 implementation, and
// it is unchanged by this file. At protocol 2 the same slots hold v2PendingBackend, which
// answers ErrV2NotBuilt until the v2 backends exist.

// ErrV2NotBuilt is what a network method answers while the node runs protocol 2 and the v2
// backends are not built yet.
var ErrV2NotBuilt = errors.New("v2 ещё не собран")

// netBackend is the network and access domain.
type netBackend interface {
	AddBootstrapNode(entry string) error
	AddRelay(link string) error
	CheckProxy(addr string) error
	ConnectToPeer(addr string) error
	CreatePass(r PassRequest) (PassView, error)
	DeletePass(id string) error
	ExtendPass(id string, r PassRequest) (PassView, error)
	ForgetInvite(peerID string) error
	GetAccessStatus() (AccessStatus, error)
	GetAnonymity() (AnonymityStatus, error)
	GetBootstrapNodes() (BootstrapStatus, error)
	GetGuardStatus() (GuardStatus, error)
	GetNetworkStats() NetworkStats
	GetPass(id string) (PassView, error)
	GetPeers() []dto.PeerDTO
	GetProxySettings() (ProxyStatus, error)
	GetRelaySettings() (RelaySettingsDTO, error)
	GetRelayStatus() (RelayStatus, error)
	JoinInvite(link string) (string, error)
	ListPasses() ([]PassView, error)
	MeasureUploadCapacity() (int, error)
	OnPeerState(cb func(PeerStateDTO)) func()
	ParseInvite(link string) (InviteInfo, error)
	PeerDiscoveryHandler() <-chan dto.PeerDTO
	PinGuard(entry string) error
	RemoveBootstrapNode(entry string) error
	RemoveJoinedPeer(peerID string) error
	RemovePassDevice(id, peerID string) error
	RevokePass(id string) (PassView, error)
	RotateAccessSecret() (string, error)
	SetAccessMode(mode string) error
	SetAnonymity(mode string, walletDirect bool) error
	SetGuardMode(mode string) error
	SetProxySettings(proxy, callRoute string) error
	SetRelaySettings(r RelaySettingsDTO) error
	StartLocalDiscovery(myAlias string)
	StartVPNVia(target string) error
	StopLocalDiscovery()
	UnpinGuard(entry string) error
}

// contactBackend is the contacts domain.
type contactBackend interface {
	AcceptMessageRequest(peerID, alias string) error
	BanContact(peerID string) error
	BlockPeer(peerID string) error
	CanRing(peerID string) bool
	DeleteContact(peerID string) error
	DismissMessageRequest(peerID string) error
	GetBlockedPeers() ([]contacts.Blocked, error)
	GetContacts() ([]dto.ContactDTO, error)
	GetMessageRequests() ([]contacts.Request, error)
	NewContact(contact dto.ContactDTO) error
	SetContactProfile(peerID, alias, avatarBase64 string) error
	UnblockPeer(peerID string) error
}

// chatBackend is the chats domain.
type chatBackend interface {
	ChatAttachments(ref ChatRef) (ChatAttachments, error)
	ChatDelete(ref ChatRef, id string) error
	ChatEdit(ref ChatRef, id, text string) error
	ChatHistory(ref ChatRef, cursor string, limit int) (ChatPage, error)
	ChatMarkRead(ref ChatRef, id string) error
	ChatMessage(ref ChatRef, id string) (ChatMessage, error)
	ChatPin(ref ChatRef, id string, on bool) error
	ChatPins(ref ChatRef) ([]ChatMessage, error)
	ChatReact(ref ChatRef, id, emoji string, on bool) error
	ChatRows() ([]ConversationDTO, error)
	ChatSend(ref ChatRef, req SendRequest) (ChatMessage, error)
	ChatThread(ref ChatRef, root, cursor string, limit int) (ChatPage, error)
	Conversations() ([]ConversationDTO, error)
	GetMessages(contactID string, limit int, beforeTimestamp uint64) ([]dto.MessageDTO, error)
	ListSavedMessages() ([]dto.SavedMessageDTO, error)
	MessageHandler() <-chan *dto.MessageDTO
	OnMessageState(cb func(MessageStateDTO))
	ResetEncryptionSession(peerID string) error
	SaveMessage(m dto.SavedMessageDTO) (dto.SavedMessageDTO, error)
	SendMessage(pubKeyHex string, text string) error
	SubscribeConversations(cb func(ConvChange))
	UnsaveMessage(id string) error
}

// groupBackend is the groups and channels domain.
type groupBackend interface {
	AddGroupMember(groupID, pubKeyHex string) error
	ChatCategoriesSetup(ref ChatRef, textName, voiceName string) ([]string, error)
	ChatChannelMove(ref ChatRef, id string, index int) error
	ChatChannelPlace(ref ChatRef, id, parent string) error
	ChatChannelRemove(ref ChatRef, id string) error
	ChatChannelSave(ref ChatRef, spec ChannelSpec) (ChannelDTO, error)
	ChatChannels(ref ChatRef) ([]ChannelDTO, error)
	ChatGroupPhoto(ref ChatRef, photoBase64 string) error
	ChatMainChannel(ref ChatRef) (ChannelDTO, error)
	ChatMainChannelSave(ref ChatRef, spec ChannelSpec) error
	ChatThreadTitle(ref ChatRef, root, title string) error
	ChatThreads(ref ChatRef) ([]ChatMessage, error)
	CreateGroup(title, description, kind string) (GroupDTO, error)
	DeleteGroup(groupID string) error
	GetGroupInvite(groupID string) (string, error)
	GetGroupMessages(groupID string, limit int) ([]GroupMessageDTO, error)
	GroupInviteContacts(groupID string, peerIDs []string, role string) (int, error)
	GroupInviteCreate(groupID string, spec GroupInviteSpec) (GroupInviteDTO, error)
	GroupInviteRevoke(groupID, nonce string) error
	GroupInvites(groupID string) ([]GroupInviteDTO, error)
	GroupJoinDecide(groupID, peerID string, accept bool) error
	GroupJoinRequests(groupID string) ([]GroupJoinRequestDTO, error)
	GroupModerationLog(groupID string) ([]GroupModEntry, error)
	GroupRoleRemove(groupID, roleID string) error
	GroupRoleSave(groupID string, r GroupRoleDTO) (GroupRoleDTO, error)
	GroupRoles(groupID string) ([]GroupRoleDTO, error)
	GroupSnapshotExport(groupID, path, password string) error
	GroupSnapshotImport(path, password string) (GroupSnapshotInfo, error)
	GroupTimeout(groupID, memberID string, minutes int) error
	InspectGroupInvite(text string) (GroupInvitePreview, error)
	JoinGroup(invite string) (GroupDTO, error)
	LeaveGroup(groupID string) error
	ListGroupMembers(groupID string) ([]GroupMemberDTO, error)
	ListGroups() ([]GroupDTO, error)
	RemoveGroupMember(groupID, memberID string, ban bool) error
	SendGroupMessage(groupID, text string) (GroupMessageDTO, error)
	SetGroupRole(groupID, memberID, role string) error
	SubscribeGroupEvents(cb func(GroupEvent))
	TransferGroupOwnership(groupID, memberID string) error
}

// fileBackend is the files domain.
type fileBackend interface {
	AcceptFile(id string) (dto.FileTransferDTO, error)
	CancelFile(id string) error
	DeclineFile(id string) error
	DownloadDir() string
	FileTransferPath(id string) (string, error)
	GetFileTransfer(id string) (dto.FileTransferDTO, error)
	ListFileTransfers(peerIDHex string) ([]dto.FileTransferDTO, error)
	OnFileTransfer(cb func(dto.FileTransferDTO)) func()
	SendFile(pubKeyHex string, path string) (dto.FileTransferDTO, error)
	SendGroupFile(groupID, channel, thread, path string) (GroupMessageDTO, error)
}

// callBackend is the calls and streams domain.
type callBackend interface {
	ActiveGroupCall(groupID string) (GroupCall, bool)
	CallStreamStats(streamID string) (StreamStatsDTO, error)
	CaptureRTPBudget(streamID string) int
	EndGroupCall(groupID, callID string) error
	EndRoomCall(groupID, room, callID string) error
	GetCallManager() *call.CallManager
	GetCallPath() CallPath
	GetCallRelays() []CallRelay
	GetCallRoster() dto.CallRosterDTO
	GetCallSlots() []dto.CallMediaSlotDTO
	InviteToCall(pubKeyHex string) (CallInvite, error)
	JoinCall(callID string, coordPeerID string) (string, error)
	KeepGroupCallAlive(groupID, callID, coordinator string) error
	KeepRoomCallAlive(groupID, room, callID, coordinator string) error
	LeaveCall() error
	LeaveRoomCall(groupID, room, callID string) error
	ListStreamQualities() []dto.StreamQualityDTO
	PublishCaptureRTP(role string, packet []byte)
	ReportCallMember(peerHex, reason string) error
	RequestRelayChange(streamID string) error
	SetCallBackdrop(name string) error
	SetCaptureEncoder(role string, enc CaptureEncoder) error
	SetDisplayRate(hz int)
	SetUploadCapacity(kbps int) error
	SetVADState(speaking bool, audioDb float64)
	SetupCallCallbacks(
		onActiveSpeaker func(speaker string, speaking bool, db float32),
		onStreamAnnounced func(streamID string, broadcaster string, mediaType string, bitrate int),
		onStreamClosed func(streamID string),
		onTopologyUpdated func(epoch uint64, coordinator string),
	)
	SetupCallMediaCallbacks(onSlot func(dto.CallMediaSlotDTO), onKicked func(reason string))
	SetupStreamInfoCallback(fn func(info dto.StreamInfoDTO))
	StartGroupCall(groupID string) (GroupCall, error)
	StartRoomCall(groupID, room string) (GroupCall, error)
	StartScreenShare(streamID string, bitrate int) error
	StartStream(streamID, mediaType, qualityID string) error
	StopScreenShare(streamID string) error
	SubmitCallAnswer(sdpAnswer string) error
	SwitchCallRelay(via string) error
	UnwatchStream(streamID string) error
	UpdateStreamQuality(streamID, qualityID string) error
	WatchStream(streamID string) error
}

// vpnBackend is the VPN, market and exit payment domain.
type vpnBackend interface {
	CheckSites(exitPeerID string, sites []string) ([]coreNode.SiteCheck, error)
	ClearVPNHistory() error
	ConnectLastExit() error
	ForgetExit(peer string) error
	GetExitFreePeers() []string
	GetExitGatewayStatus() ExitGatewayStatus
	GetExitPayment() ExitPaymentStatus
	GetProbeDomains() []string
	GetRelayMarket() []RelayInfo
	GetSavedExits() []SavedExit
	GetSharingStatus() SharingStatus
	GetSiteLists() (SiteLists, error)
	GetVPNAutoSwitch() bool
	GetVPNHistory() []VPNSession
	GetVPNStatus() (st VPNStatus)
	KillSwitchOn() bool
	ListExitNodes() []ExitNode
	PayExit(ctx context.Context, amountUSDT string) (string, error)
	ProbeExitNode(peerID string) (ProbeReport, error)
	RefreshExitNodes() (int, error)
	SetAutoPay(enabled bool, chunkUSDT, dailyCapUSDT string) error
	SetCustomSites(sites []string) error
	SetExitFavorite(peer string, on bool) error
	SetExitFreePeers(ids []string) error
	SetExitGatewayEnabled(on bool) error
	SetExitName(peer, name string) error
	SetKillSwitch(on bool) error
	SetPayAsset(asset string) error
	SetProbeDomains(domains []string) error
	SetVPNAutoSwitch(on bool) error
	SetWholeComputer(on bool) error
	StartSharing(cfg SharingConfig) error
	StartVPN(relayPeerID string) error
	StartVPNAt(relayPeerID, listen string) error
	StopSharing() error
	StopVPN() error
	StopVPNForSwitch() error
	WholeComputerSupported() bool
}

// backends holds the implementation chosen for each network domain at start-up.
type backends struct {
	net     netBackend
	contact contactBackend
	chat    chatBackend
	group   groupBackend
	file    fileBackend
	call    callBackend
	vpn     vpnBackend
}

// chooseBackends returns the v1 implementation (the facade itself) for protocol 1, and the
// not-built v2 backend for protocol 2.
func chooseBackends(protocol int, v1 *NodeApi) backends {
	if protocol == config.ProtocolV2 {
		pending := v2PendingBackend{}
		return backends{net: pending, contact: pending, chat: pending, group: pending, file: pending, call: pending, vpn: pending}
	}
	return backends{net: v1, contact: v1, chat: v1, group: v1, file: v1, call: v1, vpn: v1}
}

var (
	_ netBackend     = (*NodeApi)(nil)
	_ netBackend     = v2PendingBackend{}
	_ contactBackend = (*NodeApi)(nil)
	_ contactBackend = v2PendingBackend{}
	_ chatBackend    = (*NodeApi)(nil)
	_ chatBackend    = v2PendingBackend{}
	_ groupBackend   = (*NodeApi)(nil)
	_ groupBackend   = v2PendingBackend{}
	_ fileBackend    = (*NodeApi)(nil)
	_ fileBackend    = v2PendingBackend{}
	_ callBackend    = (*NodeApi)(nil)
	_ callBackend    = v2PendingBackend{}
	_ vpnBackend     = (*NodeApi)(nil)
	_ vpnBackend     = v2PendingBackend{}
)
