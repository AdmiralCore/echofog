package node

import (
	"context"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/stretchr/testify/require"
)

// closeFake adds the closing of a group to the fake app layer.
type closeFake struct {
	*fakeMsg
	closed []string
}

func (c *closeFake) GroupClose(_ context.Context, id string) error {
	c.closed = append(c.closed, id)
	return c.fail
}

func TestV2DeleteGroupClosesIt(t *testing.T) {
	api, f := newMsgApi(t)
	c := &closeFake{fakeMsg: f}
	api.AttachV2Messenger(c)
	require.NoError(t, api.DeleteGroup(groupID))
	require.Equal(t, []string{groupID}, c.closed)
	f.fail = app.ErrNoRight
	require.Equal(t, app.UserMessage(app.ErrNoRight), api.DeleteGroup(groupID).Error())
}

func TestV2ClosedGroupIsReadOnlyAndTellsIt(t *testing.T) {
	api, f := newMsgApi(t)
	f.rights.Member, f.rights.Send = true, false
	f.groups = []app.GroupInfo{{ID: groupID, Title: "Команда", Closed: true}}
	gs, err := api.ListGroups()
	require.NoError(t, err)
	require.True(t, gs[0].Closed)
	require.True(t, gs[0].ReadOnly)
}

func TestV2RemovedByTheOwnersCloseKeepsItsReason(t *testing.T) {
	api, f := newMsgApi(t)
	f.groups = []app.GroupInfo{{ID: groupID, Title: "Команда"}}
	var got []GroupEvent
	api.SubscribeGroupEvents(func(e GroupEvent) { got = append(got, e) })
	f.pushGroup(app.GroupEvent{Kind: "removed", Group: groupID, Reason: "deleted"})
	require.Len(t, got, 1)
	require.Equal(t, GroupEvent{Kind: "removed", GroupID: groupID, Title: "Команда", Reason: "deleted"}, got[0])
}
