package main

import (
	"context"
	"errors"

	api "github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

var errNodeNotStarted = errors.New("node not started")

func (a *App) CreateGroup(title string, description string, kind string) (api.GroupDTO, error) {
	n := a.getAPI()
	if n == nil {
		return api.GroupDTO{}, errNodeNotStarted
	}
	return n.CreateGroup(title, description, kind)
}

func (a *App) ListGroups() ([]api.GroupDTO, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	return n.ListGroups()
}

func (a *App) GetGroupInvite(groupID string) (string, error) {
	n := a.getAPI()
	if n == nil {
		return "", errNodeNotStarted
	}
	return n.GetGroupInvite(groupID)
}

func (a *App) JoinGroup(invite string) (api.GroupDTO, error) {
	n := a.getAPI()
	if n == nil {
		return api.GroupDTO{}, errNodeNotStarted
	}
	return n.JoinGroup(invite)
}

func (a *App) AddGroupMember(groupID string, pubKeyHex string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.AddGroupMember(groupID, pubKeyHex)
}

// SetGroupRole gives a member the role admin, moderator, member or restricted.
func (a *App) SetGroupRole(groupID, memberID, role string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.SetGroupRole(groupID, memberID, role)
}

// RemoveGroupMember kicks a member out of the group, or bans them.
func (a *App) RemoveGroupMember(groupID, memberID string, ban bool) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.RemoveGroupMember(groupID, memberID, ban)
}

// LeaveGroup leaves the group and forgets it on this computer.
func (a *App) LeaveGroup(groupID string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.LeaveGroup(groupID)
}

// StartGroupCall tells the members that a call opens; the caller then joins it as coordinator.
func (a *App) StartGroupCall(groupID string) (api.GroupCall, error) {
	n := a.getAPI()
	if n == nil {
		return api.GroupCall{}, errNodeNotStarted
	}
	return n.StartGroupCall(groupID)
}

// ActiveGroupCall is the call that a member recently started in the group, or nil if there is none.
func (a *App) ActiveGroupCall(groupID string) *api.GroupCall {
	n := a.getAPI()
	if n == nil {
		return nil
	}
	if c, ok := n.ActiveGroupCall(groupID); ok {
		return &c
	}
	return nil
}

// KeepGroupCallAlive tells the group the call is still going; the UI calls it while in a group call.
func (a *App) KeepGroupCallAlive(groupID, callID, coordinator string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.KeepGroupCallAlive(groupID, callID, coordinator)
}

// TransferGroupOwnership hands the group to another member; the owner becomes an admin.
func (a *App) TransferGroupOwnership(groupID, memberID string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.TransferGroupOwnership(groupID, memberID)
}

// DeleteGroup removes the group for everybody. Only the owner can.
func (a *App) DeleteGroup(groupID string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.DeleteGroup(groupID)
}

// EndGroupCall tells the group that its call is over; call it when the last person leaves.
func (a *App) EndGroupCall(groupID, callID string) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.EndGroupCall(groupID, callID)
}

func (a *App) ListGroupMembers(groupID string) ([]api.GroupMemberDTO, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	return n.ListGroupMembers(groupID)
}

func (a *App) SendGroupMessage(groupID string, text string) (api.GroupMessageDTO, error) {
	n := a.getAPI()
	if n == nil {
		return api.GroupMessageDTO{}, errNodeNotStarted
	}
	return n.SendGroupMessage(groupID, text)
}

func (a *App) GetGroupMessages(groupID string, limit int) ([]api.GroupMessageDTO, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	return n.GetGroupMessages(groupID, limit)
}

func (a *App) InviteToCall(pubKeyHex string) (map[string]string, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	inv, err := n.InviteToCall(pubKeyHex)
	if err != nil {
		return nil, err
	}
	return map[string]string{"callId": inv.CallID, "coordinator": inv.Coordinator, "messageId": inv.MessageID, "state": inv.State}, nil
}

func (a *App) GetSettings() (api.SettingsDTO, error) {
	n := a.getAPI()
	if n == nil {
		return api.SettingsDTO{}, errNodeNotStarted
	}
	return n.GetSettings()
}

func (a *App) SetSettings(s api.SettingsDTO) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.SetSettings(s)
}

func (a *App) emitIncomingCall(ctx context.Context, msg *dto.MessageDTO) {
	if msg == nil || msg.IsMine {
		return
	}
	callID, coordinator, ok := api.ParseCallInvite(msg.Text)
	if !ok {
		return
	}
	// a blocked peer, a stranger (unless everyone may call) or anybody when nobody may: no ring
	if n := a.getAPI(); n == nil || !n.CanRing(msg.SenderID) {
		return
	}
	safeEventsEmit(ctx, "call:incoming", map[string]interface{}{
		"callId":      callID,
		"coordinator": coordinator,
		"from":        msg.SenderID,
		"timestamp":   msg.Timestamp,
	})
}

// PickAndSendGroupFile lets the user choose a file and posts it in the group: every member gets
// the offer and fetches the file from this computer. Nil without error: the dialog was closed.
func (a *App) PickAndSendGroupFile(groupID string) (*api.GroupMessageDTO, error) {
	n := a.getAPI()
	if n == nil {
		return nil, errNodeNotStarted
	}
	a.mu.RLock()
	ctx := a.ctx
	a.mu.RUnlock()
	path, err := runtime.OpenFileDialog(ctx, runtime.OpenDialogOptions{Title: "Send a file"})
	if err != nil || path == "" {
		return nil, err
	}
	m, err := n.SendGroupFile(groupID, path)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// SendGroupFile posts a file (dropped on the window) in the group.
func (a *App) SendGroupFile(groupID string, path string) (api.GroupMessageDTO, error) {
	n := a.getAPI()
	if n == nil {
		return api.GroupMessageDTO{}, errNodeNotStarted
	}
	return n.SendGroupFile(groupID, path)
}
