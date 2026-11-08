package node

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/stretchr/testify/require"
)

// fakeGC stands for the calls of groups in the app layer.
type fakeGC struct {
	v2GroupCalls

	mu    sync.Mutex
	calls []string
	info  app.GroupCallInfo
	have  bool
	fail  error
	sent  app.SendReq
	ref   app.ChatRef
}

func (f *fakeGC) note(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fakeGC) called(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == s {
			return true
		}
	}
	return false
}

func (f *fakeGC) StartGroupCall(_ context.Context, g string) (app.GroupCallInfo, error) {
	f.note("start:" + g)
	return f.info, f.fail
}
func (f *fakeGC) StartRoomCall(_ context.Context, g, room string) (app.GroupCallInfo, error) {
	f.note("startroom:" + g + ":" + room)
	return f.info, f.fail
}
func (f *fakeGC) ActiveGroupCall(g, room string) (app.GroupCallInfo, bool) { return f.info, f.have }
func (f *fakeGC) KeepGroupCallAlive(_ context.Context, g, room, id, coord string) error {
	f.note("keep:" + g + ":" + room + ":" + id + ":" + coord)
	return f.fail
}
func (f *fakeGC) EndGroupCall(_ context.Context, g, room, id string) error {
	f.note("end:" + g + ":" + room + ":" + id)
	return f.fail
}
func (f *fakeGC) LeaveRoomCall(_ context.Context, g, room, id string) error {
	f.note("leave:" + g + ":" + room + ":" + id)
	return f.fail
}
func (f *fakeGC) ChatSend(_ context.Context, ref app.ChatRef, req app.SendReq) (convlog.Message, error) {
	f.mu.Lock()
	f.sent, f.ref = req, ref
	f.mu.Unlock()
	if f.fail != nil {
		return convlog.Message{}, f.fail
	}
	return convlog.Message{ID: "m-call", State: "offline", Text: req.Text}, nil
}

func newGCApi(t *testing.T) (*NodeApi, *fakeGC) {
	t.Helper()
	api, _ := newV2CallApi(t)
	f := &fakeGC{info: app.GroupCallInfo{CallID: "call_abc", Coordinator: strings.Repeat("c1", 32), From: strings.Repeat("a2", 32),
		Members: []string{strings.Repeat("a2", 32)}, StartedAt: 100, LastSeen: 120}, have: true}
	api.AttachV2GroupCalls(f)
	return api, f
}

func TestV2GroupCallsMapTheCallsOfAGroup(t *testing.T) {
	api, f := newGCApi(t)
	want := GroupCall{CallID: "call_abc", Coordinator: strings.Repeat("c1", 32), From: strings.Repeat("a2", 32), Members: []string{strings.Repeat("a2", 32)},
		StartedAt: 100, LastSeen: 120}
	c, err := api.StartGroupCall("g1")
	require.NoError(t, err)
	require.Equal(t, want, c)
	require.True(t, f.called("start:g1"))
	got, ok := api.ActiveGroupCall("g1")
	require.True(t, ok)
	require.Equal(t, want, got)
	f.have = false
	_, ok = api.ActiveGroupCall("g1")
	require.False(t, ok)

	require.NoError(t, api.KeepGroupCallAlive("g1", "call_abc", "coord"))
	require.NoError(t, api.EndGroupCall("g1", "call_abc"))
	require.True(t, f.called("keep:g1::call_abc:coord"))
	require.True(t, f.called("end:g1::call_abc"))
}

func TestV2RoomCallsMapTheCallsOfAVoiceRoom(t *testing.T) {
	api, f := newGCApi(t)
	f.info.Room = "room"
	c, err := api.StartRoomCall("g1", "room")
	require.NoError(t, err)
	require.Equal(t, "room", c.Room)
	require.True(t, f.called("startroom:g1:room"))
	require.NoError(t, api.KeepRoomCallAlive("g1", "room", "call_abc", "coord"))
	require.NoError(t, api.EndRoomCall("g1", "room", "call_abc"))
	require.NoError(t, api.LeaveRoomCall("g1", "room", "call_abc"))
	for _, s := range []string{"keep:g1:room:call_abc:coord", "end:g1:room:call_abc", "leave:g1:room:call_abc"} {
		require.True(t, f.called(s), s)
	}
}

func TestV2GroupCallErrorsAreUserTexts(t *testing.T) {
	api, f := newGCApi(t)
	f.fail = app.ErrNoRight
	_, err := api.StartGroupCall("g1")
	require.Equal(t, app.UserMessage(app.ErrNoRight), err.Error())
	require.NotContains(t, err.Error(), "app:")
}

func TestV2InviteToCallIsAChatMessageWithTheSecretOfTheCall(t *testing.T) {
	api, f := newGCApi(t)
	node := strings.Repeat("b2", 32)
	inv, err := api.InviteToCall(node)
	require.NoError(t, err)
	require.Equal(t, app.ChatRef{Kind: "dm", ID: node}, f.ref)
	require.True(t, strings.HasPrefix(f.sent.Text, convlog.CallInvitePrefix+"call_"), f.sent.Text)
	require.Equal(t, "m-call", inv.MessageID)
	require.Equal(t, "offline", inv.State)
	require.True(t, strings.HasPrefix(inv.CallID, "call_"))
	require.NotEmpty(t, inv.Coordinator)
	require.Equal(t, convlog.CallInvitePrefix+inv.CallID+"/"+inv.Coordinator, f.sent.Text, "the invitation of v1: secret and coordinator")
}

func TestV2GroupEventsOfCalls(t *testing.T) {
	api, f := newMsgApi(t)
	f.groups = []app.GroupInfo{{ID: groupID, Title: "Команда"}}
	var got []GroupEvent
	var mu sync.Mutex
	api.SubscribeGroupEvents(func(e GroupEvent) { mu.Lock(); got = append(got, e); mu.Unlock() })
	f.pushGroup(app.GroupEvent{Kind: "call", Group: groupID, From: nodeBob, CallID: "call_1", Coordinator: "co"})
	f.pushGroup(app.GroupEvent{Kind: "callactive", Group: groupID, From: nodeBob, CallID: "call_1", Coordinator: "co", Room: "room"})
	f.pushGroup(app.GroupEvent{Kind: "callend", Group: groupID, CallID: "call_1", Room: "room"})
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []GroupEvent{
		{Kind: "call", GroupID: groupID, Title: "Команда", From: nodeBob, CallID: "call_1", Coordinator: "co"},
		{Kind: "callactive", GroupID: groupID, Title: "Команда", From: nodeBob, CallID: "call_1", Coordinator: "co", Room: "room"},
		{Kind: "callend", GroupID: groupID, Title: "Команда", CallID: "call_1", Room: "room"},
	}, got)
}

func TestV2ChannelsShowTheCallOfAVoiceRoom(t *testing.T) {
	api, f := newMsgApi(t)
	f.rights = convlog.Rights{Member: true, Voice: true}
	f.room = &app.GroupCallInfo{Room: "talk", CallID: "call_9", Coordinator: "co", From: nodeBob, Members: []string{nodeBob}, StartedAt: 5, LastSeen: 6}
	chs, err := api.ChatChannels(ChatRef{Kind: "group", ID: groupID})
	require.NoError(t, err)
	require.Equal(t, "talk", chs[0].ID)
	require.NotNil(t, chs[0].Call)
	require.Equal(t, GroupCall{Room: "talk", CallID: "call_9", Coordinator: "co", From: nodeBob, Members: []string{nodeBob}, StartedAt: 5, LastSeen: 6}, *chs[0].Call)
	require.Nil(t, chs[1].Call, "a text channel has no call")
}
