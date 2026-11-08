package node

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/config"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// T11.4b: a personal call invitation reaches the UI from the v2 layer. In v1 the desktop read it from the
// message channel; at protocol 2 that channel is closed and a message arrives as a change of its chat.

type ringLog struct {
	mu  sync.Mutex
	got []IncomingCall
}

func (r *ringLog) add(c IncomingCall) { r.mu.Lock(); r.got = append(r.got, c); r.mu.Unlock() }
func (r *ringLog) all() []IncomingCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]IncomingCall(nil), r.got...)
}

const ringInvite = convlog.CallInvitePrefix + "call_s3cret/coordinator"

func fixedNow(t *testing.T) time.Time {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	old := v2Now
	v2Now = func() time.Time { return now }
	t.Cleanup(func() { v2Now = old })
	return now
}

func dmChange(msg convlog.Message, entry app.ConvEntry) app.ConvChange {
	entry.Ref = app.ChatRef{Kind: "dm", ID: nodeBob}
	return app.ConvChange{Ref: entry.Ref, Conversation: entry, Messages: []convlog.Message{msg}}
}

func inviteFrom(id string, at time.Time) convlog.Message {
	return convlog.Message{ID: id, Author: peerBob, Text: ringInvite, TS: at.UnixMilli()}
}

func newRingApi(t *testing.T) (*NodeApi, *fakeMsg, *ringLog) {
	t.Helper()
	api, f := newMsgApi(t)
	l := &ringLog{}
	api.OnIncomingCall(l.add)
	return api, f, l
}

func TestV2ARecentInvitationFromAContactRings(t *testing.T) {
	now := fixedNow(t)
	_, f, l := newRingApi(t)
	f.push(dmChange(inviteFrom("m1", now.Add(-3*time.Second)), app.ConvEntry{}))
	require.Equal(t, []IncomingCall{{CallID: "call_s3cret", Coordinator: "coordinator", From: nodeBob,
		Timestamp: now.Add(-3 * time.Second).UnixMilli()}}, l.all())
}

func TestV2AnInvitationThatWaitedIsMissedNotRinging(t *testing.T) {
	now := fixedNow(t)
	_, f, l := newRingApi(t)
	f.push(dmChange(inviteFrom("m1", now.Add(-10*time.Minute)), app.ConvEntry{}))
	got := l.all()
	require.Len(t, got, 1)
	require.True(t, got[0].Missed, "from a relay's mailbox, long after the call: it only shows as missed")
	require.Equal(t, "call_s3cret", got[0].CallID)
}

func TestV2TheSameInvitationRingsOnce(t *testing.T) {
	now := fixedNow(t)
	_, f, l := newRingApi(t)
	m := inviteFrom("m1", now)
	f.push(dmChange(m, app.ConvEntry{}))
	m.State = "delivered" // the same message again: a changed state, a reload, an edit
	f.push(dmChange(m, app.ConvEntry{}))
	f.push(dmChange(m, app.ConvEntry{}))
	require.Len(t, l.all(), 1)
	f.push(dmChange(inviteFrom("m2", now), app.ConvEntry{}))
	require.Len(t, l.all(), 2, "another invitation is another call")
}

func TestV2OnlyAnInvitationFromTheOtherSideInADirectChatRings(t *testing.T) {
	now := fixedNow(t)
	_, f, l := newRingApi(t)
	mine := inviteFrom("m1", now)
	mine.Mine = true
	f.push(dmChange(mine, app.ConvEntry{}))
	text := inviteFrom("m2", now)
	text.Text = "позвони мне fogcall://nothing"
	f.push(dmChange(text, app.ConvEntry{}))
	bad := inviteFrom("m3", now)
	bad.Text = convlog.CallInvitePrefix + "call_x" // no coordinator
	f.push(dmChange(bad, app.ConvEntry{}))
	gone := inviteFrom("m4", now)
	gone.Deleted = true
	f.push(dmChange(gone, app.ConvEntry{}))
	grp := dmChange(inviteFrom("m5", now), app.ConvEntry{})
	grp.Ref = app.ChatRef{Kind: "group", ID: groupID}
	grp.Conversation.Ref = grp.Ref
	f.push(grp)
	require.Empty(t, l.all())
}

func TestV2ABlockedContactAndAStrangerDoNotRingByDefault(t *testing.T) {
	now := fixedNow(t)
	_, f, l := newRingApi(t)
	f.push(dmChange(inviteFrom("m1", now), app.ConvEntry{Blocked: true}))
	f.push(dmChange(inviteFrom("m2", now), app.ConvEntry{Request: true}))
	require.Empty(t, l.all())
}

func TestV2CallPrivacyDecidesWhoRings(t *testing.T) {
	now := fixedNow(t)
	api, f, l := newRingApi(t)
	require.NoError(t, api.node.SetPrivacy(coreNode.Privacy{Messages: coreNode.WhoEveryone, Calls: coreNode.WhoEveryone}))
	f.push(dmChange(inviteFrom("m1", now), app.ConvEntry{Request: true}))
	require.Len(t, l.all(), 1, "everyone may call: a stranger rings")
	f.push(dmChange(inviteFrom("m2", now), app.ConvEntry{Blocked: true}))
	require.Len(t, l.all(), 1, "a blocked contact never rings")
	require.NoError(t, api.node.SetPrivacy(coreNode.Privacy{Messages: coreNode.WhoEveryone, Calls: coreNode.WhoNobody}))
	f.push(dmChange(inviteFrom("m3", now), app.ConvEntry{}))
	require.Len(t, l.all(), 1, "nobody may call: not even a contact")
}

func TestV2ASubscriptionMadeBeforeTheLayerIsAttachedIsKept(t *testing.T) {
	now := fixedNow(t)
	api := newFacadeNode(t, config.ProtocolV2)
	l := &ringLog{}
	api.OnIncomingCall(l.add) // the desktop subscribes at start-up
	f := &fakeMsg{}
	api.AttachV2Messenger(f)
	f.push(dmChange(inviteFrom("m1", now), app.ConvEntry{}))
	require.Len(t, l.all(), 1)
}

func TestV2ANewSubscriptionReplacesTheOldAndNilStops(t *testing.T) {
	now := fixedNow(t)
	api, f, first := newRingApi(t)
	second := &ringLog{}
	api.OnIncomingCall(second.add)
	f.push(dmChange(inviteFrom("m1", now), app.ConvEntry{}))
	require.Empty(t, first.all())
	require.Len(t, second.all(), 1)
	api.OnIncomingCall(nil)
	f.push(dmChange(inviteFrom("m2", now), app.ConvEntry{}))
	require.Len(t, second.all(), 1)
	require.Equal(t, 0, f.live(), "nothing is left subscribed to the chats")
}

func TestV1HasNoIncomingCallEventsToGive(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV1)
	l := &ringLog{}
	api.OnIncomingCall(l.add) // v1 rings from MessageHandler: this stays silent and does not fail
	require.Empty(t, l.all())
}
