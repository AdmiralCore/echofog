package node

import (
	"context"
	"sync"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/pkg/dto"
)

// v2Messenger is the part of *app.App the protocol 2 chats, groups and files backends use (T11.3d).
// A test stands in for the app by embedding it and overriding the methods it needs.
type v2Messenger interface {
	// chats
	Conversations() ([]app.ConvEntry, error)
	ChatHistory(ctx context.Context, ref app.ChatRef, cursor string, limit int) (app.ChatPage, error)
	ChatThread(ctx context.Context, ref app.ChatRef, root, cursor string, limit int) (app.ChatPage, error)
	ChatPins(ctx context.Context, ref app.ChatRef) ([]convlog.Message, error)
	ChatMessage(ctx context.Context, ref app.ChatRef, id string) (convlog.Message, error)
	ChatSend(ctx context.Context, ref app.ChatRef, req app.SendReq) (convlog.Message, error)
	ChatEdit(ctx context.Context, ref app.ChatRef, id, text string) error
	ChatDelete(ctx context.Context, ref app.ChatRef, id string) error
	ChatReact(ctx context.Context, ref app.ChatRef, id, emoji string, on bool) error
	ChatPin(ctx context.Context, ref app.ChatRef, id string, on bool) error
	ChatMarkRead(ctx context.Context, ref app.ChatRef, id string) error
	SubscribeChats(fn func(app.ConvChange)) (cancel func())
	NodeOfPeer(peer string) (string, bool)
	ChatNotify(ref app.ChatRef) (string, error)
	SetChatNotify(ref app.ChatRef, mode string) error
	ChatFollow(ref app.ChatRef, root string, on bool) error
	ChatFollowing(ref app.ChatRef) ([]string, error)
	ChatMuteUntil(ref app.ChatRef, until int64) error
	ChatMutedUntil(ref app.ChatRef) (int64, error)
	SaveMessage(m app.SavedMessage) (app.SavedMessage, error)
	UnsaveMessage(id string) error
	ListSavedMessages() ([]app.SavedMessage, error)

	// groups
	CreateGroup(ctx context.Context, title, desc, kind string) (app.GroupInfo, error)
	Groups() ([]app.GroupInfo, error)
	GroupInfoOf(id string) (app.GroupInfo, error)
	LeaveGroup(ctx context.Context, id string) error
	GroupMembers(id string) ([]app.GroupMember, error)
	GroupSetRole(ctx context.Context, id, node, role string) error
	GroupTransfer(ctx context.Context, id, node string) error
	GroupRemove(ctx context.Context, id, node string, ban bool) error
	GroupTimeout(ctx context.Context, id, node string, minutes int) error
	GroupRoles(id string) ([]app.GroupRole, error)
	GroupRoleSave(ctx context.Context, id string, r app.GroupRole) (app.GroupRole, error)
	GroupRoleRemove(ctx context.Context, id, role string) error
	GroupRights(id, channel string) (convlog.Rights, error)
	GroupModerationLog(id string) ([]convlog.ModEntry, error)
	GroupChannels(id string) ([]convlog.Channel, error)
	GroupChannelSave(ctx context.Context, id string, s convlog.ChannelSpec) (convlog.Channel, error)
	GroupChannelRemove(ctx context.Context, id, ch string) error
	GroupChannelMove(ctx context.Context, id, ch string, index int) error
	GroupChannelPlace(ctx context.Context, id, ch, parent string) error
	GroupCategoriesSetup(ctx context.Context, id, text, voice string) ([]string, error)
	GroupMainChannel(id string) (convlog.Channel, error)
	GroupMainChannelSave(ctx context.Context, id string, s convlog.ChannelSpec) error
	GroupPhoto(ctx context.Context, id string, photo []byte) error
	ChatThreads(ctx context.Context, ref app.ChatRef) ([]convlog.Message, error)
	ChatThreadTitle(ctx context.Context, ref app.ChatRef, root, title string) error
	GroupInviteCreate(ctx context.Context, id string, s app.InviteSpec) (app.GroupInvite, error)
	GroupInvites(id string) ([]app.GroupInvite, error)
	GroupInviteRevoke(ctx context.Context, id, nonce string) error
	GroupInviteContacts(ctx context.Context, id string, nodes []string, role string) (int, error)
	InspectGroupInvite(link string) (app.InvitePreview, error)
	JoinGroup(ctx context.Context, link string) (app.GroupInfo, error)
	GroupJoinRequests(id string) ([]app.JoinRequest, error)
	GroupJoinDecide(ctx context.Context, id, node string, accept bool) error
	GroupVoice(id, channel string) ([]app.VoiceMember, error)
	GroupAddMember(ctx context.Context, id, node string) error
	GroupChannelStats(id string) (map[string]app.ChannelStat, error)
	GroupSnapshotExport(ctx context.Context, id, path, password string) error
	GroupSnapshotImport(ctx context.Context, path, password string) (app.GroupInfo, int, error)
	SubscribeGroupEvents(fn func(app.GroupEvent)) (cancel func())
	ActiveGroupCall(group, room string) (app.GroupCallInfo, bool)

	// files
	OnFileTransfer(cb func(app.Transfer)) func()
	SendFile(ctx context.Context, peerHex, path string) (app.Transfer, error)
	SendGroupFile(ctx context.Context, group string, members [][32]byte, path string) (app.Transfer, error)
	AcceptFile(id string) (app.Transfer, error)
	DeclineFile(id string) error
	CancelFile(id string) error
	GetFileTransfer(id string) (app.Transfer, error)
	ListFileTransfers(peerHex string) ([]app.Transfer, error)
	FileTransferPath(id string) (string, error)
	DownloadDir() string
}

var _ v2Messenger = (*app.App)(nil)

// v2Subs remembers what the UI subscribed to before the v2 layer was attached: the desktop
// subscribes once at start-up, and the app layer may come later. AttachV2Messenger replays them.
type v2Subs struct {
	mu    sync.Mutex
	conv  func(ConvChange)
	group func(GroupEvent)
	file  func(dto.FileTransferDTO)
	state func(MessageStateDTO)
	ring  func(IncomingCall)
	b     *v2MsgBackend // set once attached
}

func (s *v2Subs) rememberConv(cb func(ConvChange)) {
	s.mu.Lock()
	s.conv = cb
	s.mu.Unlock()
}

func (s *v2Subs) rememberGroup(cb func(GroupEvent)) {
	s.mu.Lock()
	s.group = cb
	s.mu.Unlock()
}

func (s *v2Subs) rememberFile(cb func(dto.FileTransferDTO)) {
	s.mu.Lock()
	s.file = cb
	s.mu.Unlock()
}

func (s *v2Subs) rememberState(cb func(MessageStateDTO)) {
	s.mu.Lock()
	s.state = cb
	s.mu.Unlock()
}

// v2MsgBackend is the protocol 2 chats, groups and files backend: it turns what the app layer says
// into the DTOs the frontend knows. What the app layer cannot do yet answers ErrV2NotBuilt through the
// embedded pending backend.
type v2MsgBackend struct {
	v2PendingBackend
	app v2Messenger

	mu          sync.Mutex
	cancelConv  func()
	cancelGroup func()
	cancelFile  func()
	cancelRing  func()
}

var (
	_ chatBackend  = (*v2MsgBackend)(nil)
	_ groupBackend = (*v2MsgBackend)(nil)
	_ fileBackend  = (*v2MsgBackend)(nil)
)

// AttachV2Messenger gives the facade the v2 chat, group and file layer: those three domains answer
// from it from now on, and what the UI subscribed to before is subscribed again. At protocol 1 it does
// nothing. Call it once, after AttachV2.
func (n *NodeApi) AttachV2Messenger(m v2Messenger) {
	if m == nil || n.node == nil || n.node.Cfg == nil || !n.node.Cfg.IsV2() {
		return
	}
	b := &v2MsgBackend{app: m}
	n.backends.chat, n.backends.group, n.backends.file = b, b, b
	n.v2m.mu.Lock()
	n.v2m.b = b
	conv, group, file, ring := n.v2m.conv, n.v2m.group, n.v2m.file, n.v2m.ring
	n.v2m.mu.Unlock()
	if conv != nil {
		b.SubscribeConversations(conv)
	}
	if group != nil {
		b.SubscribeGroupEvents(group)
	}
	if file != nil {
		b.OnFileTransfer(file)
	}
	if ring != nil {
		b.watchCalls(ring, n.callsWho)
	}
}
