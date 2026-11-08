package node

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/stretchr/testify/require"
)

// parityFake adds the calls of T11.3e-2 to the fake app layer.
type parityFake struct {
	*fakeMsg
	saved   []app.SavedMessage
	added   [][2]string
	snapExp [3]string
	snapImp [2]string
	muted   map[string]int64
	stats   map[string]app.ChannelStat
	addErr  []error
	impErr  error
	invites []app.GroupInvite
}

func (p *parityFake) SaveMessage(m app.SavedMessage) (app.SavedMessage, error) {
	if p.fail != nil {
		return app.SavedMessage{}, p.fail
	}
	m.ID, m.SavedAt = m.ChatID+"/"+m.MsgID, 1700000000123
	p.saved = append([]app.SavedMessage{m}, p.saved...)
	return m, nil
}
func (p *parityFake) UnsaveMessage(id string) error { p.note("unsave:" + id); return p.fail }
func (p *parityFake) ListSavedMessages() ([]app.SavedMessage, error) {
	return p.saved, p.fail
}
func (p *parityFake) GroupAddMember(_ context.Context, id, node string) error {
	p.added = append(p.added, [2]string{id, node})
	if len(p.addErr) > 0 {
		err := p.addErr[0]
		p.addErr = p.addErr[1:]
		return err
	}
	return p.fail
}
func (p *parityFake) GroupSnapshotExport(_ context.Context, id, path, pw string) error {
	p.snapExp = [3]string{id, path, pw}
	return p.fail
}
func (p *parityFake) GroupSnapshotImport(_ context.Context, path, pw string) (app.GroupInfo, int, error) {
	p.snapImp = [2]string{path, pw}
	if p.impErr != nil {
		return app.GroupInfo{}, 0, p.impErr
	}
	return app.GroupInfo{ID: groupID, Title: "Команда", Members: 2}, 7, nil
}
func (p *parityFake) ChatMuteUntil(ref app.ChatRef, until int64) error {
	if p.fail != nil {
		return p.fail
	}
	if p.muted == nil {
		p.muted = map[string]int64{}
	}
	p.muted[ref.ID+"/"+ref.Channel] = until
	return nil
}
func (p *parityFake) ChatMutedUntil(ref app.ChatRef) (int64, error) {
	return p.muted[ref.ID+"/"+ref.Channel], p.fail
}
func (p *parityFake) GroupChannelStats(string) (map[string]app.ChannelStat, error) {
	return p.stats, p.fail
}
func (p *parityFake) GroupInvites(string) ([]app.GroupInvite, error) { return p.invites, p.fail }

func newParityApi(t *testing.T) (*NodeApi, *parityFake) {
	t.Helper()
	api, f := newMsgApi(t)
	p := &parityFake{fakeMsg: f}
	api.AttachV2Messenger(p)
	return api, p
}

func TestV2SavedMessagesGoToTheAppAndBack(t *testing.T) {
	api, p := newParityApi(t)
	got, err := api.SaveMessage(dto.SavedMessageDTO{ChatID: "g:" + groupID, ChatName: "Команда", MsgID: "m1", Channel: "dev", Thread: "t1",
		Sender: nodeBob, Text: "текст", FileName: "a.bin", FileSize: 5, Ts: 1700000000000})
	require.NoError(t, err)
	require.Equal(t, "g:"+groupID+"/m1", got.ID)
	require.EqualValues(t, 1700000000123, got.SavedAt)
	require.Equal(t, app.SavedMessage{ID: got.ID, ChatID: "g:" + groupID, ChatName: "Команда", MsgID: "m1", Channel: "dev", Thread: "t1",
		Sender: nodeBob, Text: "текст", FileName: "a.bin", FileSize: 5, TS: 1700000000000, SavedAt: 1700000000123}, p.saved[0])
	list, err := api.ListSavedMessages()
	require.NoError(t, err)
	require.Equal(t, []dto.SavedMessageDTO{got}, list)
	require.NoError(t, api.UnsaveMessage(got.ID))
	require.True(t, p.called("unsave:"+got.ID))
	p.saved = nil
	list, err = api.ListSavedMessages()
	require.NoError(t, err)
	require.NotNil(t, list, "an empty list is [], not null")
	p.fail = app.ErrSavedFull
	_, err = api.SaveMessage(dto.SavedMessageDTO{ChatID: "d:x", MsgID: "m"})
	require.Equal(t, app.UserMessage(app.ErrSavedFull), err.Error())
}

func TestV2AddGroupMemberTakesANodeOrAnAccountKey(t *testing.T) {
	api, p := newParityApi(t)
	require.NoError(t, api.AddGroupMember(groupID, nodeBob))
	require.Equal(t, [2]string{groupID, nodeBob}, p.added[0])
	p.added, p.addErr = nil, []error{app.ErrNoContact, nil}
	key := strings.Repeat("0a", 32)
	require.NoError(t, api.AddGroupMember(groupID, key))
	require.Len(t, p.added, 2)
	require.NotEqual(t, key, p.added[1][1], "the account key is turned into the node id")
	p.fail = app.ErrAlreadyMember
	err := api.AddGroupMember(groupID, nodeBob)
	require.Equal(t, app.UserMessage(app.ErrAlreadyMember), err.Error())
}

func TestV2GroupSnapshotOverTheApp(t *testing.T) {
	api, p := newParityApi(t)
	require.NoError(t, api.GroupSnapshotExport(groupID, "/tmp/g.fogzip", "long enough pass"))
	require.Equal(t, [3]string{groupID, "/tmp/g.fogzip", "long enough pass"}, p.snapExp)
	info, err := api.GroupSnapshotImport("/tmp/g.fogzip", "long enough pass")
	require.NoError(t, err)
	require.Equal(t, 7, info.Events)
	require.Equal(t, groupID, info.Group.ID)
	require.Equal(t, "Команда", info.Group.Title)
	p.impErr = app.ErrSnapshotVersion
	_, err = api.GroupSnapshotImport("/tmp/v1.fogzip", "long enough pass")
	require.Equal(t, app.UserMessage(app.ErrSnapshotVersion), err.Error())
	require.NotEqual(t, app.UserMessage(app.ErrSnapshotFile), err.Error())
}

func TestV2MutedUntilIsPassedAndShownInTheChatList(t *testing.T) {
	api, p := newParityApi(t)
	ref := ChatRef{Kind: "group", ID: groupID}
	require.NoError(t, api.SetChatMutedUntil(ref, 1900000000))
	got, err := api.ChatMutedUntil(ref)
	require.NoError(t, err)
	require.EqualValues(t, 1900000000, got)
	p.entries = []app.ConvEntry{{Ref: app.ChatRef{Kind: "dm", ID: nodeBob}, Kind: "direct", Name: "Боб", PeerID: nodeBob, MutedUntil: 1900000001}}
	rows, err := api.Conversations()
	require.NoError(t, err)
	require.EqualValues(t, 1900000001, rows[0].MutedUntil)
	p.fail = app.ErrRequest
	require.Equal(t, app.UserMessage(app.ErrRequest), api.SetChatMutedUntil(ref, 1).Error())
}

func TestV2ChannelsCarryUnreadMentionsAndTheLastMessage(t *testing.T) {
	api, p := newParityApi(t)
	p.rights = convlog.Rights{Member: true, Send: true}
	p.stats = map[string]app.ChannelStat{
		"talk": {Unread: 3, Mentions: 1, LastTS: 1700000000500, Last: &convlog.Message{ID: eventAAA, Author: peerBob, Text: "последнее", Kind: "text"}},
	}
	got, err := api.ChatChannels(ChatRef{Kind: "group", ID: groupID})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, 3, got[0].Unread)
	require.Equal(t, 1, got[0].Mentions)
	require.EqualValues(t, 1700000000500, got[0].LastTS)
	require.NotNil(t, got[0].Last)
	require.Equal(t, "последнее", got[0].Last.Text)
	require.Equal(t, nodeBob, got[0].Last.Author, "named by the node id")
	require.Nil(t, got[1].Last)
	require.Zero(t, got[1].Unread)
}

func TestV2InvitesShowTheirWebLink(t *testing.T) {
	api, p := newParityApi(t)
	web := "https://join.example.org/join/#group/abc"
	p.invites = []app.GroupInvite{{Nonce: "n1", Link: "echofog-group:2:abc", Web: web, Exp: 4102444800, Issuer: nodeMe}}
	list, err := api.GroupInvites(groupID)
	require.NoError(t, err)
	require.Equal(t, web, list[0].WebLink)
	require.Equal(t, "echofog-group:2:abc", list[0].Link)
}

func TestV2GroupMessageEventsOnlyForNewMessages(t *testing.T) {
	api, f := newMsgApi(t)
	f.groups = []app.GroupInfo{{ID: groupID, Title: "Команда"}}
	var got []GroupEvent
	var mu sync.Mutex
	api.SubscribeGroupEvents(func(e GroupEvent) { mu.Lock(); got = append(got, e); mu.Unlock() })
	g := app.ChatRef{Kind: "group", ID: groupID}
	// an edit or a reaction: the message is in Messages (conv:change shows it) but is not new
	f.push(app.ConvChange{Ref: g, Messages: []convlog.Message{{ID: "m1", Author: peerBob, Text: "правка", Edited: true}}})
	f.push(app.ConvChange{Ref: g, Messages: []convlog.Message{{ID: "m1", Author: peerBob}, {ID: "m4", Author: peerBob}}, Fresh: []string{"m4"}})
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 1)
	require.Equal(t, GroupEvent{Kind: "message", GroupID: groupID, Title: "Команда", From: nodeBob, MessageID: "m4"}, got[0])
}
