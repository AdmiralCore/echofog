package node

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/stretchr/testify/require"
)

var (
	peerBob  = strings.Repeat("b1", 32) // the v1 peer id messages name bob by
	nodeBob  = strings.Repeat("b2", 32) // bob's node id, how the contacts and members know him
	peerMe   = strings.Repeat("a1", 32)
	nodeMe   = strings.Repeat("a2", 32)
	groupID  = "grp-1"
	eventAAA = strings.Repeat("0a", 16)
)

// fakeMsg stands for the app layer: it embeds the interface, so a call a test did not expect panics,
// and records what the backend asked.
type fakeMsg struct {
	v2Messenger

	mu       sync.Mutex
	calls    []string
	sent     app.SendReq
	sentRef  app.ChatRef
	entries  []app.ConvEntry
	msgs     []convlog.Message
	groups   []app.GroupInfo
	members  []app.GroupMember
	roles    []app.GroupRole
	rights   convlog.Rights
	fail     error
	noNext   bool
	room     *app.GroupCallInfo
	mod      []convlog.ModEntry
	chatSubs []func(app.ConvChange)
	grpSubs  []func(app.GroupEvent)
	fileCb   func(app.Transfer)
	transfer app.Transfer
	filePeer string
	fileGrp  string
	fileCh   string
	fileTh   string
}

func (f *fakeMsg) note(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeMsg) called(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == s {
			return true
		}
	}
	return false
}

func (f *fakeMsg) NodeOfPeer(p string) (string, bool) {
	switch p {
	case peerBob:
		return nodeBob, true
	case peerMe:
		return nodeMe, true
	}
	return "", false
}

func (f *fakeMsg) Conversations() ([]app.ConvEntry, error) { return f.entries, f.fail }
func (f *fakeMsg) ChatHistory(_ context.Context, ref app.ChatRef, cursor string, limit int) (app.ChatPage, error) {
	f.note("history:" + ref.Kind + ":" + ref.ID + ":" + ref.Channel + ":" + cursor)
	next := "next"
	if f.noNext {
		next = ""
	}
	return app.ChatPage{Messages: f.msgs, Next: next}, f.fail
}
func (f *fakeMsg) ChatThread(_ context.Context, ref app.ChatRef, root, cursor string, limit int) (app.ChatPage, error) {
	f.note("thread:" + root)
	return app.ChatPage{Messages: f.msgs}, f.fail
}
func (f *fakeMsg) ChatPins(_ context.Context, ref app.ChatRef) ([]convlog.Message, error) {
	return f.msgs, f.fail
}
func (f *fakeMsg) ChatMessage(_ context.Context, ref app.ChatRef, id string) (convlog.Message, error) {
	if len(f.msgs) == 0 {
		return convlog.Message{}, f.fail
	}
	return f.msgs[0], f.fail
}
func (f *fakeMsg) ChatSend(_ context.Context, ref app.ChatRef, req app.SendReq) (convlog.Message, error) {
	f.mu.Lock()
	f.sent, f.sentRef = req, ref
	f.mu.Unlock()
	if f.fail != nil {
		return convlog.Message{}, f.fail
	}
	return convlog.Message{ID: eventAAA, Author: peerMe, Mine: true, Text: req.Text, Kind: "text"}, nil
}
func (f *fakeMsg) ChatEdit(_ context.Context, _ app.ChatRef, id, text string) error {
	f.note("edit:" + id + ":" + text)
	return f.fail
}
func (f *fakeMsg) ChatDelete(_ context.Context, _ app.ChatRef, id string) error {
	f.note("delete:" + id)
	return f.fail
}
func (f *fakeMsg) ChatReact(_ context.Context, _ app.ChatRef, id, emoji string, on bool) error {
	f.note("react:" + id + ":" + emoji + map[bool]string{true: ":on", false: ":off"}[on])
	return f.fail
}
func (f *fakeMsg) ChatPin(_ context.Context, _ app.ChatRef, id string, on bool) error {
	f.note("pin:" + id + map[bool]string{true: ":on", false: ":off"}[on])
	return f.fail
}
func (f *fakeMsg) ChatMarkRead(_ context.Context, _ app.ChatRef, id string) error {
	f.note("read:" + id)
	return f.fail
}
func (f *fakeMsg) SubscribeChats(fn func(app.ConvChange)) func() {
	f.mu.Lock()
	f.chatSubs = append(f.chatSubs, fn)
	n := len(f.chatSubs) - 1
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.chatSubs[n] = nil
		f.mu.Unlock()
	}
}
func (f *fakeMsg) push(ch app.ConvChange) {
	f.mu.Lock()
	subs := append([]func(app.ConvChange){}, f.chatSubs...)
	f.mu.Unlock()
	for _, s := range subs {
		if s != nil {
			s(ch)
		}
	}
}
func (f *fakeMsg) live() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.chatSubs {
		if s != nil {
			n++
		}
	}
	return n
}

func (f *fakeMsg) Groups() ([]app.GroupInfo, error) { return f.groups, f.fail }
func (f *fakeMsg) GroupInfoOf(id string) (app.GroupInfo, error) {
	for _, g := range f.groups {
		if g.ID == id {
			return g, nil
		}
	}
	return app.GroupInfo{}, app.ErrNoGroup
}
func (f *fakeMsg) CreateGroup(_ context.Context, title, desc, kind string) (app.GroupInfo, error) {
	f.note("create:" + title + ":" + kind)
	if f.fail != nil {
		return app.GroupInfo{}, f.fail
	}
	return app.GroupInfo{ID: groupID, Title: title, Description: desc, Kind: kind, Role: "owner", Owner: true, Members: 1, Created: 100}, nil
}
func (f *fakeMsg) GroupMembers(id string) ([]app.GroupMember, error) { return f.members, f.fail }
func (f *fakeMsg) GroupRoles(id string) ([]app.GroupRole, error)     { return f.roles, f.fail }
func (f *fakeMsg) GroupRights(id, channel string) (convlog.Rights, error) {
	return f.rights, f.fail
}
func (f *fakeMsg) LeaveGroup(_ context.Context, id string) error {
	f.note("leave:" + id)
	return f.fail
}
func (f *fakeMsg) GroupSetRole(_ context.Context, id, node, role string) error {
	f.note("role:" + node + ":" + role)
	return f.fail
}
func (f *fakeMsg) GroupTransfer(_ context.Context, id, node string) error {
	f.note("transfer:" + node)
	return f.fail
}
func (f *fakeMsg) GroupRemove(_ context.Context, id, node string, ban bool) error {
	f.note("remove:" + node + map[bool]string{true: ":ban", false: ""}[ban])
	return f.fail
}
func (f *fakeMsg) GroupTimeout(_ context.Context, id, node string, minutes int) error {
	f.note("timeout:" + node)
	return f.fail
}
func (f *fakeMsg) GroupInviteCreate(_ context.Context, id string, s app.InviteSpec) (app.GroupInvite, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "invite:"+s.TTL.String()+":"+s.Role+":"+s.Label)
	f.mu.Unlock()
	return app.GroupInvite{Nonce: "n1", Role: s.Role, Label: s.Label, Issuer: nodeMe, Exp: 5000, Uses: s.Uses, Link: "echofog-group:2:xyz", Approval: s.Approval}, f.fail
}
func (f *fakeMsg) GroupInvites(id string) ([]app.GroupInvite, error) {
	return []app.GroupInvite{
		{Nonce: "a", Issuer: nodeMe, Exp: time.Now().Add(time.Hour).Unix(), Uses: 2, Used: 0, Link: "echofog-group:2:a"},
		{Nonce: "b", Issuer: nodeBob, Exp: time.Now().Add(time.Hour).Unix(), Revoked: true},
		{Nonce: "c", Issuer: nodeBob, Exp: 1},
		{Nonce: "d", Issuer: nodeBob, Exp: time.Now().Add(time.Hour).Unix(), Uses: 1, Used: 1},
	}, f.fail
}
func (f *fakeMsg) InspectGroupInvite(link string) (app.InvitePreview, error) {
	if f.fail != nil {
		return app.InvitePreview{}, f.fail
	}
	return app.InvitePreview{GroupID: groupID, Title: "Команда", Kind: app.GroupChannel, Expires: 9, Approval: true, Member: true}, nil
}
func (f *fakeMsg) JoinGroup(_ context.Context, link string) (app.GroupInfo, error) {
	if f.fail != nil {
		return app.GroupInfo{}, f.fail
	}
	return app.GroupInfo{ID: groupID, Title: "Команда", Kind: app.GroupPrivate, Role: "member", Members: 2, Pending: true}, nil
}
func (f *fakeMsg) GroupJoinRequests(id string) ([]app.JoinRequest, error) {
	return []app.JoinRequest{{Node: nodeBob, TS: 7, Invite: "n1"}}, f.fail
}
func (f *fakeMsg) GroupJoinDecide(_ context.Context, id, node string, accept bool) error {
	f.note("decide:" + node + map[bool]string{true: ":yes", false: ":no"}[accept])
	return f.fail
}
func (f *fakeMsg) GroupInviteRevoke(_ context.Context, id, nonce string) error {
	f.note("revoke:" + nonce)
	return f.fail
}
func (f *fakeMsg) GroupInviteContacts(_ context.Context, id string, nodes []string, role string) (int, error) {
	return len(nodes), f.fail
}
func (f *fakeMsg) GroupChannels(id string) ([]convlog.Channel, error) {
	return []convlog.Channel{{ID: "talk", Name: "Разговоры", Voice: true}, {ID: "news", Name: "Новости", ReadOnly: true}}, f.fail
}
func (f *fakeMsg) GroupModerationLog(id string) ([]convlog.ModEntry, error) { return f.mod, f.fail }
func (f *fakeMsg) GroupMainChannel(id string) (convlog.Channel, error) {
	return convlog.Channel{Name: "Основной", Open: "news"}, f.fail
}
func (f *fakeMsg) GroupVoice(id, channel string) ([]app.VoiceMember, error) {
	return []app.VoiceMember{{Node: nodeBob, Muted: true}}, f.fail
}

func (f *fakeMsg) OnFileTransfer(cb func(app.Transfer)) func() {
	f.mu.Lock()
	f.fileCb = cb
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.fileCb = nil
		f.mu.Unlock()
	}
}
func (f *fakeMsg) SendFile(_ context.Context, peer, path string) (app.Transfer, error) {
	f.mu.Lock()
	f.filePeer = peer
	f.mu.Unlock()
	return f.transfer, f.fail
}
func (f *fakeMsg) SendGroupFile(_ context.Context, group, channel, thread, path string) (app.Transfer, error) {
	f.mu.Lock()
	f.fileGrp, f.fileCh, f.fileTh = group, channel, thread
	f.mu.Unlock()
	return f.transfer, f.fail
}
func (f *fakeMsg) AcceptFile(id string) (app.Transfer, error)      { return f.transfer, f.fail }
func (f *fakeMsg) GetFileTransfer(id string) (app.Transfer, error) { return f.transfer, f.fail }
func (f *fakeMsg) ListFileTransfers(peer string) ([]app.Transfer, error) {
	f.note("list:" + peer)
	return []app.Transfer{f.transfer}, f.fail
}
func (f *fakeMsg) DeclineFile(id string) error { f.note("decline:" + id); return f.fail }
func (f *fakeMsg) CancelFile(id string) error  { f.note("cancel:" + id); return f.fail }
func (f *fakeMsg) FileTransferPath(id string) (string, error) {
	return "/downloads/x.bin", f.fail
}
func (f *fakeMsg) DownloadDir() string { return "/downloads" }

func newMsgApi(t *testing.T) (*NodeApi, *fakeMsg) {
	t.Helper()
	api := newFacadeNode(t, config.ProtocolV2)
	f := &fakeMsg{}
	api.AttachV2Messenger(f)
	return api, f
}

func hexNode(b byte) string {
	return strings.Repeat(string("0123456789abcdef"[b>>4])+string("0123456789abcdef"[b&15]), 32)
}

// ---- chats ---------------------------------------------------------------------------------------------------

func TestV2ChatHistoryNamesAuthorsByTheNodeId(t *testing.T) {
	api, f := newMsgApi(t)
	f.msgs = []convlog.Message{{
		ID: eventAAA, Author: peerBob, DeletedBy: peerMe, PinnedBy: peerBob, Mentions: []string{peerMe, strings.Repeat("c3", 32)},
		Reactions: []convlog.Reaction{{Emoji: "👍", Count: 2, By: []string{peerBob, peerMe}}}, Text: "привет",
	}}
	page, err := api.ChatHistory(ChatRef{Kind: "group", ID: groupID, Channel: "news"}, "cur", 20)
	require.NoError(t, err)
	require.Equal(t, "next", page.Next)
	require.Len(t, page.Messages, 1)
	m := page.Messages[0]
	require.Equal(t, nodeBob, m.Author)
	require.Equal(t, nodeMe, m.DeletedBy)
	require.Equal(t, nodeBob, m.PinnedBy)
	require.Equal(t, []string{nodeMe, strings.Repeat("c3", 32)}, m.Mentions, "an author the app does not know stays as it is")
	require.Equal(t, []string{nodeBob, nodeMe}, m.Reactions[0].By)
	require.True(t, f.called("history:group:"+groupID+":news:cur"))
	// the app's own copy is untouched: the message the log hands out is shared
	require.Equal(t, peerBob, f.msgs[0].Author)
	require.Equal(t, []string{peerBob, peerMe}, f.msgs[0].Reactions[0].By)
}

func TestV2ChatHistoryOfNothingIsAnEmptyListNotNull(t *testing.T) {
	api, _ := newMsgApi(t)
	page, err := api.ChatHistory(ChatRef{Kind: "dm", ID: nodeBob}, "", 20)
	require.NoError(t, err)
	require.NotNil(t, page.Messages)
	pins, err := api.ChatPins(ChatRef{Kind: "dm", ID: nodeBob})
	require.NoError(t, err)
	require.NotNil(t, pins)
}

func TestV2ChatSendAndOperations(t *testing.T) {
	api, f := newMsgApi(t)
	m, err := api.ChatSend(ChatRef{Kind: "group", ID: groupID, Channel: "news"}, SendRequest{Text: "всем", ReplyTo: "r1", Thread: "t1"})
	require.NoError(t, err)
	require.Equal(t, nodeMe, m.Author)
	require.Equal(t, app.SendReq{Text: "всем", ReplyTo: "r1", Thread: "t1"}, f.sent)
	require.Equal(t, app.ChatRef{Kind: "group", ID: groupID, Channel: "news"}, f.sentRef)

	ref := ChatRef{Kind: "dm", ID: nodeBob}
	require.NoError(t, api.ChatEdit(ref, "e1", "правка"))
	require.NoError(t, api.ChatDelete(ref, "e2"))
	require.NoError(t, api.ChatReact(ref, "e3", "👍", true))
	require.NoError(t, api.ChatReact(ref, "e3", "👍", false))
	require.NoError(t, api.ChatPin(ref, "e4", true))
	require.NoError(t, api.ChatMarkRead(ref, "e5"))
	for _, c := range []string{"edit:e1:правка", "delete:e2", "react:e3:👍:on", "react:e3:👍:off", "pin:e4:on", "read:e5"} {
		require.True(t, f.called(c), c)
	}
	_, err = api.ChatThread(ref, "root1", "", 10)
	require.NoError(t, err)
	require.True(t, f.called("thread:root1"))
}

func TestV2ChatErrorsReachTheUserAsRussianText(t *testing.T) {
	api, f := newMsgApi(t)
	f.fail = app.ErrNoChat
	_, err := api.ChatHistory(ChatRef{Kind: "dm", ID: nodeBob}, "", 10)
	require.Error(t, err)
	require.Equal(t, "Такого чата нет", err.Error())
	require.False(t, strings.Contains(err.Error(), "app:"))
	f.fail = errors.New("badger: disk is full at /home/x")
	err = api.ChatEdit(ChatRef{Kind: "dm", ID: nodeBob}, "e", "t")
	require.Equal(t, "Не удалось выполнить действие", err.Error(), "an internal error is never shown as it is")
}

func TestV2ConversationsBuildTheChatList(t *testing.T) {
	api, f := newMsgApi(t)
	last := convlog.Message{ID: eventAAA, Author: peerBob, Text: "последнее"}
	f.entries = []app.ConvEntry{
		{Ref: app.ChatRef{Kind: "dm", ID: nodeBob}, Conv: "c1", Last: &last, LastTS: 5, Unread: 2, Kind: "direct", Name: "Боб", PeerID: nodeBob, Online: true},
		{Ref: app.ChatRef{Kind: "dm", ID: hexNode(9)}, Conv: "c2", Request: true, Kind: "direct", Name: hexNode(9), PeerID: hexNode(9)},
		{Ref: app.ChatRef{Kind: "group", ID: groupID}, Conv: "c3", Kind: "group", Name: "Команда", Unread: 1, Mentions: 1, Upgraded: true},
	}
	f.groups = []app.GroupInfo{{ID: groupID, Title: "Команда", Role: "admin", Members: 4, Kind: app.GroupPrivate}}
	f.rights = convlog.Rights{Member: true, Send: true, Media: true, Manage: true, Invite: true, Voice: true}
	f.members = []app.GroupMember{{Node: nodeMe, Self: true, Role: "admin", Rank: 50}}
	rows, err := api.Conversations()
	require.NoError(t, err)
	require.Len(t, rows, 3)

	dm := rows[0]
	require.Equal(t, "dm", dm.Ref.Kind)
	require.Equal(t, "direct", dm.Kind)
	require.Equal(t, "Боб", dm.Name)
	require.Equal(t, nodeBob, dm.PeerID)
	require.True(t, dm.Online)
	require.Equal(t, 2, dm.Unread)
	require.Equal(t, nodeBob, dm.Last.Author, "the last message names its author by the node id too")
	require.True(t, dm.Can.Send && dm.Can.Files && dm.Can.React, "a contact may be written to")

	req := rows[1]
	require.True(t, req.Request)
	require.False(t, req.Can.Send, "a stranger's chat is read only until it is a contact")

	g := rows[2]
	require.Equal(t, "group", g.Kind)
	require.Equal(t, "Команда", g.Title)
	require.Equal(t, "admin", g.Role)
	require.Equal(t, 4, g.Members)
	require.True(t, g.Can.Send && g.Can.Files && g.Can.Manage && g.Can.Invite && g.Can.Call)
	require.False(t, g.Can.Roles || g.Can.Pin)
	require.True(t, g.Upgraded)

	chat, err := api.ChatRows()
	require.NoError(t, err)
	require.Equal(t, rows, chat, "ChatRows and Conversations are one list")
}

func TestV2ConversationsShowAPendingGroupAndItsRequests(t *testing.T) {
	api, f := newMsgApi(t)
	f.entries = []app.ConvEntry{{Ref: app.ChatRef{Kind: "group", ID: groupID}, Kind: "group", Name: "Г"}}
	f.groups = []app.GroupInfo{{ID: groupID, Title: "Г", Role: "owner", Members: 1, Pending: true}}
	f.rights = convlog.Rights{Member: true, Send: true}
	f.members = []app.GroupMember{{Node: nodeMe, Self: true, Owner: true, Rank: 100}}
	rows, err := api.Conversations()
	require.NoError(t, err)
	require.NotEmpty(t, rows[0].Pending, "a group waiting to be let in says so")
	require.False(t, rows[0].Can.Send, "nothing may be written to a group that has not let us in")

	f.groups[0].Pending = false
	rows, _ = api.Conversations()
	require.Empty(t, rows[0].Pending)
	require.Equal(t, 1, rows[0].Requests, "the requests that wait for a decision are counted")
}

func TestV2SubscribeConversationsMapsChangesAndReplacesTheOldHandler(t *testing.T) {
	api, f := newMsgApi(t)
	var got []ConvChange
	var mu sync.Mutex
	api.SubscribeConversations(func(c ConvChange) { mu.Lock(); got = append(got, c); mu.Unlock() })
	f.push(app.ConvChange{
		Ref: app.ChatRef{Kind: "dm", ID: nodeBob}, Rev: 7, Reload: true,
		Conversation: app.ConvEntry{Ref: app.ChatRef{Kind: "dm", ID: nodeBob}, Kind: "direct", Name: "Боб", PeerID: nodeBob},
		Messages:     []convlog.Message{{ID: eventAAA, Author: peerBob, Text: "новое"}},
	})
	mu.Lock()
	require.Len(t, got, 1)
	require.Equal(t, uint64(7), got[0].Rev)
	require.True(t, got[0].Reload)
	require.Equal(t, "dm", got[0].Ref.Kind)
	require.Equal(t, nodeBob, got[0].Messages[0].Author)
	require.Equal(t, "Боб", got[0].Conversation.Name)
	mu.Unlock()

	// a second subscription replaces the first one: no change is delivered twice
	var second int
	api.SubscribeConversations(func(ConvChange) { mu.Lock(); second++; mu.Unlock() })
	require.Equal(t, 1, f.live())
	f.push(app.ConvChange{Ref: app.ChatRef{Kind: "dm", ID: nodeBob}})
	mu.Lock()
	require.Equal(t, 1, second)
	require.Len(t, got, 1)
	mu.Unlock()
}

// The desktop subscribes once, when it starts; the app layer may come later.
func TestV2SubscriptionsMadeBeforeTheAttachAreKept(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	var conv, group, file int
	var mu sync.Mutex
	api.SubscribeConversations(func(ConvChange) { mu.Lock(); conv++; mu.Unlock() })
	api.SubscribeGroupEvents(func(GroupEvent) { mu.Lock(); group++; mu.Unlock() })
	api.OnFileTransfer(func(dtoFileTransfer) { mu.Lock(); file++; mu.Unlock() })
	f := &fakeMsg{}
	api.AttachV2Messenger(f)
	f.push(app.ConvChange{Ref: app.ChatRef{Kind: "group", ID: groupID}, Messages: []convlog.Message{{ID: eventAAA, Author: peerBob, Text: "x"}}, Fresh: []string{eventAAA}})
	f.fileCb(app.Transfer{ID: "f1", State: app.FileDone})
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, conv)
	require.Equal(t, 1, group, "a message in a group is a group event")
	require.Equal(t, 1, file)
}

func TestV2SubscribeGroupEventsTellsMessagesAndMembership(t *testing.T) {
	api, f := newMsgApi(t)
	var got []GroupEvent
	var mu sync.Mutex
	api.SubscribeGroupEvents(func(e GroupEvent) { mu.Lock(); got = append(got, e); mu.Unlock() })
	g := app.ChatRef{Kind: "group", ID: groupID}
	f.groups = []app.GroupInfo{{ID: groupID, Title: "Команда"}}
	f.push(app.ConvChange{Ref: g, Messages: []convlog.Message{{ID: "m1", Author: peerBob, Text: "привет"}}, Fresh: []string{"m1"}})
	f.push(app.ConvChange{Ref: g, Messages: []convlog.Message{{ID: "m2", Author: peerMe, Mine: true}}, Fresh: []string{"m2"}}) // own: no event
	f.push(app.ConvChange{Ref: g, Reload: true})
	f.push(app.ConvChange{Ref: app.ChatRef{Kind: "dm", ID: nodeBob}, Messages: []convlog.Message{{ID: "m3", Author: peerBob}}, Fresh: []string{"m3"}}) // a direct chat is no group
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 2)
	require.Equal(t, GroupEvent{Kind: "message", GroupID: groupID, Title: "Команда", From: nodeBob, MessageID: "m1"}, got[0])
	require.Equal(t, GroupEvent{Kind: "members", GroupID: groupID, Title: "Команда"}, got[1])
}

func TestV2LegacyMessageCallsGoThroughTheChat(t *testing.T) {
	api, f := newMsgApi(t)
	require.NoError(t, api.SendMessage(nodeBob, "по старому"))
	require.Equal(t, app.ChatRef{Kind: "dm", ID: nodeBob}, f.sentRef)
	require.Equal(t, "по старому", f.sent.Text)
	// the message channel of v1 stays closed: v2 tells of new messages through conv:change
	_, open := <-api.MessageHandler()
	require.False(t, open)
	// the reset of a session is not built (the sessions of v2 are not reset by hand)
	require.ErrorIs(t, api.ResetEncryptionSession(nodeBob), ErrV2NotBuilt)
}

func TestV2ChatAttachmentsSortFilesMediaAndLinks(t *testing.T) {
	api, f := newMsgApi(t)
	f.noNext = true
	f.msgs = []convlog.Message{
		{ID: "1", Author: peerBob, Kind: "file", File: &convlog.FileRef{Name: "a.png", Mime: "image/png"}},
		{ID: "2", Author: peerBob, Kind: "file", File: &convlog.FileRef{Name: "b.pdf", Mime: "application/pdf"}},
		{ID: "3", Author: peerBob, Text: "смотри https://example.org/x. и дальше"},
		{ID: "4", Author: peerBob, Deleted: true, Text: "https://deleted.example"},
	}
	got, err := api.ChatAttachments(ChatRef{Kind: "dm", ID: nodeBob})
	require.NoError(t, err)
	require.Len(t, got.Media, 1)
	require.Len(t, got.Files, 1)
	require.Len(t, got.Links, 1)
	require.Equal(t, "https://example.org/x", got.Links[0].URL)
	require.Equal(t, nodeBob, got.Links[0].Author)
}

// ---- groups --------------------------------------------------------------------------------------------------

func TestV2CreateGroupAndListUseTheKindsOfV1(t *testing.T) {
	api, f := newMsgApi(t)
	f.members = []app.GroupMember{{Node: nodeMe, Self: true, Owner: true, Rank: 100, Role: "owner"}}
	g, err := api.CreateGroup("Команда", "рабочий чат", "group")
	require.NoError(t, err)
	require.True(t, f.called("create:Команда:private"), "the kind the UI says is translated for the app")
	require.Equal(t, GroupDTO{ID: groupID, Title: "Команда", Description: "рабочий чат", Kind: "group", ReadOnly: false,
		Creator: nodeMe, CreatedAt: 100, Members: 1, MyRole: "owner"}, g)
	_, err = api.CreateGroup("Канал", "", "channel")
	require.NoError(t, err)
	require.True(t, f.called("create:Канал:channel"))
	_, err = api.CreateGroup("Рой", "", "swarm")
	require.NoError(t, err)
	require.True(t, f.called("create:Рой:open"))

	f.groups = []app.GroupInfo{{ID: "g2", Title: "Открытая", Kind: app.GroupOpen, Role: "member", Members: 9, Created: 1}}
	f.members = []app.GroupMember{{Node: nodeBob, Owner: true, Rank: 100}}
	list, err := api.ListGroups()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "swarm", list[0].Kind)
	require.Equal(t, nodeBob, list[0].Creator)
	require.Equal(t, "member", list[0].MyRole)
}

func TestV2ListGroupMembersAndWhatICanDoToThem(t *testing.T) {
	api, f := newMsgApi(t)
	f.roles = []app.GroupRole{
		{ID: "owner", Rank: 100, Perms: convlog.PermAll, Builtin: true},
		{ID: "admin", Rank: 50, Perms: convlog.PermKick | convlog.PermBan | convlog.PermManageRoles, Builtin: true},
		{ID: "member", Rank: 10, Builtin: true},
	}
	f.members = []app.GroupMember{
		{Node: nodeMe, Self: true, Role: "admin", Rank: 50, Title: "Админ", Online: true},
		{Node: nodeBob, Role: "member", Rank: 10, Timeout: 99},
		{Node: hexNode(7), Role: "owner", Rank: 100, Owner: true},
	}
	list, err := api.ListGroupMembers(groupID)
	require.NoError(t, err)
	require.Len(t, list, 3)
	me, bob, owner := list[0], list[1], list[2]
	require.True(t, me.IsMe)
	require.True(t, me.Online)
	require.Equal(t, "Админ", me.RoleTitle)
	require.False(t, me.CanKick || me.CanBan || me.CanTransfer, "nothing is done to oneself")
	require.True(t, bob.CanKick && bob.CanBan && bob.CanTimeout, "an admin acts on a member")
	require.False(t, bob.CanTransfer, "only the owner hands the group over")
	require.Equal(t, int64(99), bob.Timeout)
	require.Contains(t, bob.Roles, "member")
	require.False(t, owner.CanKick || owner.CanBan || owner.CanTimeout, "nobody acts on the owner")
	require.Empty(t, owner.Roles)
	require.Equal(t, nodeBob, bob.PeerID)
}

func TestV2GroupMemberActionsReachTheApp(t *testing.T) {
	api, f := newMsgApi(t)
	require.NoError(t, api.SetGroupRole(groupID, nodeBob, "admin"))
	require.NoError(t, api.TransferGroupOwnership(groupID, nodeBob))
	require.NoError(t, api.RemoveGroupMember(groupID, nodeBob, false))
	require.NoError(t, api.RemoveGroupMember(groupID, nodeBob, true))
	require.NoError(t, api.GroupTimeout(groupID, nodeBob, 10))
	require.NoError(t, api.LeaveGroup(groupID))
	for _, c := range []string{"role:" + nodeBob + ":admin", "transfer:" + nodeBob, "remove:" + nodeBob, "remove:" + nodeBob + ":ban", "timeout:" + nodeBob, "leave:" + groupID} {
		require.True(t, f.called(c), c)
	}
}

func TestV2GroupInvites(t *testing.T) {
	api, f := newMsgApi(t)
	inv, err := api.GroupInviteCreate(groupID, GroupInviteSpec{Label: "друзья", Hours: 48, Uses: 3, Role: "member", Approval: true})
	require.NoError(t, err)
	require.True(t, f.called("invite:48h0m0s:member:друзья"))
	require.Equal(t, "n1", inv.N)
	require.True(t, inv.Approval)
	require.Equal(t, "echofog-group:2:xyz", inv.Link)
	require.True(t, inv.Mine)

	list, err := api.GroupInvites(groupID)
	require.NoError(t, err)
	states := map[string]string{}
	for _, i := range list {
		states[i.N] = i.State
	}
	require.Equal(t, map[string]string{"a": "active", "b": "revoked", "c": "expired", "d": "used"}, states)
	require.True(t, list[0].Mine && list[0].CanRevoke)
	require.False(t, list[1].Mine)

	require.NoError(t, api.GroupInviteRevoke(groupID, "a"))
	require.True(t, f.called("revoke:a"))
	n, err := api.GroupInviteContacts(groupID, []string{nodeBob}, "member")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	link, err := api.GetGroupInvite(groupID)
	require.NoError(t, err)
	require.Equal(t, "echofog-group:2:xyz", link)
}

func TestV2InspectAndJoinAGroup(t *testing.T) {
	api, f := newMsgApi(t)
	p, err := api.InspectGroupInvite("echofog-group:2:xyz")
	require.NoError(t, err)
	require.Equal(t, GroupInvitePreview{GroupID: groupID, Title: "Команда", Kind: "channel", Approval: true, Exp: 9, Joined: true}, p)
	g, err := api.JoinGroup("echofog-group:2:xyz")
	require.NoError(t, err)
	require.Equal(t, groupID, g.ID)
	require.Equal(t, "group", g.Kind)

	f.fail = app.ErrInvite
	_, e1 := api.InspectGroupInvite("junk")
	_, e2 := api.JoinGroup("junk")
	require.Equal(t, e1.Error(), e2.Error(), "an invitation that does not work reads the same everywhere")
	require.Equal(t, "Ссылка-приглашение не подходит", e1.Error())
}

func TestV2JoinRequests(t *testing.T) {
	api, f := newMsgApi(t)
	list, err := api.GroupJoinRequests(groupID)
	require.NoError(t, err)
	require.Equal(t, []GroupJoinRequestDTO{{PeerID: nodeBob, TS: 7, Invite: "n1"}}, list)
	require.NoError(t, api.GroupJoinDecide(groupID, nodeBob, true))
	require.NoError(t, api.GroupJoinDecide(groupID, nodeBob, false))
	require.True(t, f.called("decide:"+nodeBob+":yes") && f.called("decide:"+nodeBob+":no"))
}

func TestV2GroupRolesAndModerationLog(t *testing.T) {
	api, f := newMsgApi(t)
	f.roles = []app.GroupRole{
		{ID: "owner", Rank: 100, Perms: convlog.PermAll, Builtin: true},
		{ID: "admin", Title: "Админ", Color: 0xff0000, Rank: 50, Perms: convlog.PermKick | convlog.PermManageRoles, Builtin: true},
		{ID: "helper", Title: "Помощник", Rank: 30, Perms: convlog.PermSend},
		{ID: "member", Rank: 10, Builtin: true},
	}
	f.members = []app.GroupMember{{Node: nodeMe, Self: true, Role: "admin", Rank: 50}, {Node: nodeBob, Role: "helper", Rank: 30}}
	roles, err := api.GroupRoles(groupID)
	require.NoError(t, err)
	require.Len(t, roles, 4)
	byID := map[string]GroupRoleDTO{}
	for _, r := range roles {
		byID[r.ID] = r
	}
	require.Equal(t, 1, byID["helper"].Members, "members of the role are counted")
	require.True(t, byID["helper"].CanEdit && byID["helper"].CanRemove, "a role below mine that is not built in can be changed and removed")
	require.False(t, byID["admin"].CanEdit, "my own rank is not edited by me")
	require.False(t, byID["owner"].CanEdit || byID["owner"].CanRemove)
	require.False(t, byID["member"].CanRemove, "a built-in role stays")
	require.Equal(t, uint32(0xff0000), byID["admin"].Color)

	f.mod = []convlog.ModEntry{{TS: 1, Actor: peerMe, Action: "ban", Target: peerBob}, {TS: 2, Actor: peerBob, Action: "role", Target: strings.Repeat("c3", 32)}}
	log, err := api.GroupModerationLog(groupID)
	require.NoError(t, err)
	require.Equal(t, nodeMe, log[0].Actor)
	require.Equal(t, nodeBob, log[0].Target)
	require.Equal(t, strings.Repeat("c3", 32), log[1].Target, "a person the app does not know keeps the id")
	require.Equal(t, peerMe, f.mod[0].Actor, "the log of the app is not rewritten")
}

func TestV2ChannelsCarryWhatICanDoInThem(t *testing.T) {
	api, f := newMsgApi(t)
	f.rights = convlog.Rights{Member: true, Send: true, Voice: true}
	chs, err := api.ChatChannels(ChatRef{Kind: "group", ID: groupID})
	require.NoError(t, err)
	require.Len(t, chs, 2)
	require.Equal(t, "talk", chs[0].ID)
	require.True(t, chs[0].Voice)
	require.True(t, chs[0].CanJoin, "a voice room is joined, not written to")
	require.True(t, chs[1].ReadOnly)
	main, err := api.ChatMainChannel(ChatRef{Kind: "group", ID: groupID})
	require.NoError(t, err)
	require.Equal(t, "news", main.Open)
}

func TestV2GroupMessagesOfV1GoThroughTheChat(t *testing.T) {
	api, f := newMsgApi(t)
	f.msgs = []convlog.Message{{ID: "m1", Author: peerBob, Text: "привет", TS: 1500}}
	list, err := api.GetGroupMessages(groupID, 10)
	require.NoError(t, err)
	require.Equal(t, []GroupMessageDTO{{ID: "m1", GroupID: groupID, Sender: nodeBob, Text: "привет", Timestamp: 1500, Verified: true}}, list)
	m, err := api.SendGroupMessage(groupID, "ответ")
	require.NoError(t, err)
	require.Equal(t, nodeMe, m.Sender)
	require.True(t, m.IsMine)
	require.Equal(t, app.ChatRef{Kind: "group", ID: groupID}, f.sentRef)
}

func TestV2WhatTheAppCannotDoYetIsNotBuilt(t *testing.T) {
	api, _ := newMsgApi(t)
	// the reset of a session has no counterpart in v2: the sessions are not reset by hand
	require.ErrorIs(t, api.ResetEncryptionSession(nodeBob), ErrV2NotBuilt)
}

// ---- files ---------------------------------------------------------------------------------------------------

type dtoFileTransfer = dto.FileTransferDTO

func TestV2FilesMapTransfers(t *testing.T) {
	api, f := newMsgApi(t)
	now := time.UnixMilli(1700000000123)
	f.transfer = app.Transfer{ID: "f1", Peer: nodeBob, Dir: app.DirIn, Name: "a.bin", Size: 10, Mime: "x/y", State: app.FileActive, Done: 4,
		Rate: 2, Stalled: true, Group: groupID, Members: 3, Delivered: 1, Path: "/p", Created: now, Updated: now.Add(time.Second)}
	got, err := api.GetFileTransfer("f1")
	require.NoError(t, err)
	require.Equal(t, dtoFileTransfer{ID: "f1", Peer: nodeBob, Dir: "in", Name: "a.bin", Size: 10, Mime: "x/y", State: "active", Path: "/p", Done: 4,
		Rate: 2, Stalled: true, Group: groupID, Members: 3, Delivered: 1, Created: 1700000000123, Updated: 1700000001123}, got)

	tr, err := api.AcceptFile("f1")
	require.NoError(t, err)
	require.Equal(t, "f1", tr.ID)
	list, err := api.ListFileTransfers(nodeBob)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.True(t, f.called("list:"+nodeBob))
	require.NoError(t, api.DeclineFile("f1"))
	require.NoError(t, api.CancelFile("f1"))
	require.True(t, f.called("decline:f1") && f.called("cancel:f1"))
	p, err := api.FileTransferPath("f1")
	require.NoError(t, err)
	require.Equal(t, "/downloads/x.bin", p)
	require.Equal(t, "/downloads", api.DownloadDir())
}

func TestV2SendFileTakesANodeOrAnAccountKeyAndSendGroupFileNamesTheChannelAndThread(t *testing.T) {
	api, f := newMsgApi(t)
	f.transfer = app.Transfer{ID: "f2", Name: "b.bin", Size: 5, Group: groupID}
	_, err := api.SendFile(nodeBob, "/tmp/b.bin")
	require.NoError(t, err)
	require.Equal(t, nodeBob, f.filePeer)

	f.members = []app.GroupMember{{Node: nodeMe, Self: true}, {Node: nodeBob}, {Node: hexNode(7)}}
	m, err := api.SendGroupFile(groupID, "news", "t1", "/tmp/b.bin")
	require.NoError(t, err)
	require.Equal(t, groupID, f.fileGrp)
	require.Equal(t, "news", f.fileCh, "the channel the caller chose goes to the app")
	require.Equal(t, "t1", f.fileTh, "the thread too")
	require.Equal(t, GroupMessageDTO{ID: "f2", GroupID: groupID, Sender: nodeMe, Text: "b.bin", IsMine: true, Verified: true,
		File: &GroupFileDTO{ID: "f2", Name: "b.bin", Size: 5, Channel: "news", Thread: "t1"}}, withoutTime(m))
}

func withoutTime(m GroupMessageDTO) GroupMessageDTO { m.Timestamp = 0; return m }

func TestV2OnFileTransferDeliversDTOsAndCancels(t *testing.T) {
	api, f := newMsgApi(t)
	var got []dtoFileTransfer
	var mu sync.Mutex
	stop := api.OnFileTransfer(func(d dtoFileTransfer) { mu.Lock(); got = append(got, d); mu.Unlock() })
	require.NotNil(t, stop)
	f.fileCb(app.Transfer{ID: "f3", Dir: app.DirOut, State: app.FileDone})
	mu.Lock()
	require.Len(t, got, 1)
	require.Equal(t, "f3", got[0].ID)
	require.Equal(t, "done", got[0].State)
	mu.Unlock()
	stop()
	require.Nil(t, f.fileCb)
}

// ---- T7.7 on the facade --------------------------------------------------------------------------------------

func TestV2NotifyFollowAndVoiceOnTheFacade(t *testing.T) {
	api, f := newMsgApi(t)
	_ = f
	ref := ChatRef{Kind: "group", ID: groupID, Channel: "news"}
	// the settings are the app's; the facade passes them on
	f2 := &notifyFake{fakeMsg: f}
	api.AttachV2Messenger(f2)
	require.NoError(t, api.SetChatNotify(ref, "mentions"))
	require.Equal(t, "mentions", f2.mode)
	mode, err := api.ChatNotify(ref)
	require.NoError(t, err)
	require.Equal(t, "mentions", mode)
	require.NoError(t, api.ChatFollow(ref, "root1", true))
	list, err := api.ChatFollowing(ref)
	require.NoError(t, err)
	require.Equal(t, []string{"root1"}, list)
	room, err := api.GroupVoice(groupID, "talk")
	require.NoError(t, err)
	require.Equal(t, []VoiceMemberDTO{{PeerID: nodeBob, Muted: true}}, room)
}

type notifyFake struct {
	*fakeMsg
	mode string
	fol  []string
}

func (n *notifyFake) ChatNotify(app.ChatRef) (string, error)      { return n.mode, nil }
func (n *notifyFake) SetChatNotify(_ app.ChatRef, m string) error { n.mode = m; return nil }
func (n *notifyFake) ChatFollow(_ app.ChatRef, r string, on bool) error {
	n.fol = append(n.fol, r)
	return nil
}
func (n *notifyFake) ChatFollowing(app.ChatRef) ([]string, error) { return n.fol, nil }

// At protocol 1 the v2 facade calls say plainly that they are not there.
func TestV2OnlyCallsAnswerNotBuiltAtProtocol1(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	_, err := api.ChatNotify(ChatRef{Kind: "dm", ID: nodeBob})
	require.ErrorIs(t, err, ErrV2NotBuilt)
	require.ErrorIs(t, api.SetChatNotify(ChatRef{Kind: "dm", ID: nodeBob}, "all"), ErrV2NotBuilt)
	_, err = api.GroupVoice(groupID, "talk")
	require.ErrorIs(t, err, ErrV2NotBuilt)
}

func TestAttachV2MessengerDoesNothingAtProtocol1(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	api.AttachV2Messenger(&fakeMsg{})
	require.IsType(t, &v1ChatBackend{}, api.backends.chat)
	require.IsType(t, &v1GroupBackend{}, api.backends.group)
	require.IsType(t, &v1FileBackend{}, api.backends.file)
}

func (f *fakeMsg) SubscribeGroupEvents(fn func(app.GroupEvent)) func() {
	f.mu.Lock()
	f.grpSubs = append(f.grpSubs, fn)
	n := len(f.grpSubs) - 1
	f.mu.Unlock()
	return func() { f.mu.Lock(); f.grpSubs[n] = nil; f.mu.Unlock() }
}

func (f *fakeMsg) pushGroup(ev app.GroupEvent) {
	f.mu.Lock()
	subs := append([]func(app.GroupEvent){}, f.grpSubs...)
	f.mu.Unlock()
	for _, s := range subs {
		if s != nil {
			s(ev)
		}
	}
}

// Every kind of group event the frontend listens to (stores/chats.js) is told, with the fields it reads.
func TestV2GroupEventsOfTheAppReachTheFrontendKinds(t *testing.T) {
	api, f := newMsgApi(t)
	f.groups = []app.GroupInfo{{ID: groupID, Title: "Команда"}}
	var got []GroupEvent
	var mu sync.Mutex
	api.SubscribeGroupEvents(func(e GroupEvent) { mu.Lock(); got = append(got, e); mu.Unlock() })
	f.pushGroup(app.GroupEvent{Kind: "requests", Group: groupID, From: nodeBob})
	f.pushGroup(app.GroupEvent{Kind: "requests", Group: groupID})
	f.pushGroup(app.GroupEvent{Kind: "added", Group: groupID})
	f.pushGroup(app.GroupEvent{Kind: "removed", Group: groupID, Reason: "banned"})
	f.pushGroup(app.GroupEvent{Kind: "history", Group: groupID})
	f.pushGroup(app.GroupEvent{Kind: "bogus", Group: groupID})
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []GroupEvent{
		{Kind: "requests", GroupID: groupID, Title: "Команда", From: nodeBob},
		{Kind: "requests", GroupID: groupID, Title: "Команда"},
		{Kind: "added", GroupID: groupID, Title: "Команда"},
		{Kind: "removed", GroupID: groupID, Title: "Команда", Reason: "banned"},
		{Kind: "history", GroupID: groupID, Title: "Команда"},
	}, got, "an unknown kind is not passed on")
}

// A group that is gone still names itself in the removed event: the app told the title before it forgot it.
func TestV2RemovedEventKeepsTheTitleOfAGroupThatIsGone(t *testing.T) {
	api, f := newMsgApi(t)
	f.groups = nil
	var got []GroupEvent
	api.SubscribeGroupEvents(func(e GroupEvent) { got = append(got, e) })
	f.pushGroup(app.GroupEvent{Kind: "removed", Group: groupID, Title: "Команда", Reason: "removed"})
	require.Equal(t, []GroupEvent{{Kind: "removed", GroupID: groupID, Title: "Команда", Reason: "removed"}}, got)
}

func (f *fakeMsg) ActiveGroupCall(group, room string) (app.GroupCallInfo, bool) {
	if f.room != nil && room == f.room.Room {
		return *f.room, true
	}
	return app.GroupCallInfo{}, false
}

// the news of the channels: none unless a test brings them
func (f *fakeMsg) GroupChannelStats(string) (map[string]app.ChannelStat, error) { return nil, nil }
