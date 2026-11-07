package node

import (
	"context"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
)

// v2GroupCalls is the part of *app.App the calls of groups and the invitation to a call use (T11.4).
type v2GroupCalls interface {
	StartGroupCall(ctx context.Context, group string) (app.GroupCallInfo, error)
	StartRoomCall(ctx context.Context, group, room string) (app.GroupCallInfo, error)
	ActiveGroupCall(group, room string) (app.GroupCallInfo, bool)
	KeepGroupCallAlive(ctx context.Context, group, room, callID, coordinator string) error
	EndGroupCall(ctx context.Context, group, room, callID string) error
	LeaveRoomCall(ctx context.Context, group, room, callID string) error
	ChatSend(ctx context.Context, ref app.ChatRef, req app.SendReq) (convlog.Message, error)
}

var _ v2GroupCalls = (*app.App)(nil)

// AttachV2GroupCalls (stub of the red commit).
func (n *NodeApi) AttachV2GroupCalls(g v2GroupCalls) {}
