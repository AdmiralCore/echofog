package app

import (
	"context"
	"errors"
)

// GroupCallInfo is a call going on in a group (stub of the red commit).
type GroupCallInfo struct {
	Room, CallID, Coordinator, From string
	Members                         []string
	StartedAt, LastSeen             int64
}

// StartGroupCall starts the call of a group (stub).
func (a *App) StartGroupCall(ctx context.Context, group string) (GroupCallInfo, error) {
	return GroupCallInfo{}, errors.New("app: not built")
}

// StartRoomCall starts the call of a voice room (stub).
func (a *App) StartRoomCall(ctx context.Context, group, room string) (GroupCallInfo, error) {
	return GroupCallInfo{}, errors.New("app: not built")
}

// ActiveGroupCall is the call going on in a group or one of its rooms (stub).
func (a *App) ActiveGroupCall(group, room string) (GroupCallInfo, bool) {
	return GroupCallInfo{}, false
}

// KeepGroupCallAlive tells the others the call goes on (stub).
func (a *App) KeepGroupCallAlive(ctx context.Context, group, room, callID, coordinator string) error {
	return errors.New("app: not built")
}

// EndGroupCall ends the call of a group or a room (stub).
func (a *App) EndGroupCall(ctx context.Context, group, room, callID string) error {
	return errors.New("app: not built")
}

// LeaveRoomCall says this node left a room whose call goes on (stub).
func (a *App) LeaveRoomCall(ctx context.Context, group, room, callID string) error {
	return errors.New("app: not built")
}

// ForgetGroupCalls drops what is known of the calls of the groups (stub).
func (a *App) ForgetGroupCalls() {}
