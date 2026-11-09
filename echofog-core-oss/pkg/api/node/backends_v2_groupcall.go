package node

import (
	"context"

	"github.com/echofog/echofog-core/internal/services/call"
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

// AttachV2GroupCalls gives the calls backend the calls of groups and the invitation to a call. Call it
// after AttachV2Calls; at protocol 1 or without that, it does nothing.
func (n *NodeApi) AttachV2GroupCalls(g v2GroupCalls) {
	if g == nil {
		return
	}
	if b, ok := n.backends.call.(*v2CallBackend); ok {
		b.mu.Lock()
		b.gc = g
		b.mu.Unlock()
	}
}

func (b *v2CallBackend) groupCalls() (v2GroupCalls, error) {
	b.mu.Lock()
	g := b.gc
	b.mu.Unlock()
	if g == nil {
		return nil, ErrV2NotBuilt
	}
	return g, nil
}

func groupCall(i app.GroupCallInfo) GroupCall {
	return GroupCall{Room: i.Room, Members: i.Members, CallID: i.CallID, Coordinator: i.Coordinator, From: i.From, StartedAt: i.StartedAt, LastSeen: i.LastSeen}
}

func (b *v2CallBackend) StartGroupCall(groupID string) (GroupCall, error) {
	return b.start(groupID, "")
}

func (b *v2CallBackend) StartRoomCall(groupID, room string) (GroupCall, error) {
	return b.start(groupID, room)
}

func (b *v2CallBackend) start(groupID, room string) (GroupCall, error) {
	g, err := b.groupCalls()
	if err != nil {
		return GroupCall{}, err
	}
	ctx, cancel := v2Ctx()
	defer cancel()
	var i app.GroupCallInfo
	if room == "" {
		i, err = g.StartGroupCall(ctx, groupID)
	} else {
		i, err = g.StartRoomCall(ctx, groupID, room)
	}
	if err != nil {
		return GroupCall{}, v2Err(err)
	}
	return groupCall(i), nil
}

func (b *v2CallBackend) ActiveGroupCall(groupID string) (GroupCall, bool) {
	g, err := b.groupCalls()
	if err != nil {
		return GroupCall{}, false
	}
	i, ok := g.ActiveGroupCall(groupID, "")
	if !ok {
		return GroupCall{}, false
	}
	return groupCall(i), true
}

func (b *v2CallBackend) keep(groupID, room, callID, coordinator string) error {
	g, err := b.groupCalls()
	if err != nil {
		return err
	}
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(g.KeepGroupCallAlive(ctx, groupID, room, callID, coordinator))
}

func (b *v2CallBackend) KeepGroupCallAlive(groupID, callID, coordinator string) error {
	return b.keep(groupID, "", callID, coordinator)
}

func (b *v2CallBackend) KeepRoomCallAlive(groupID, room, callID, coordinator string) error {
	return b.keep(groupID, room, callID, coordinator)
}

func (b *v2CallBackend) end(groupID, room, callID string) error {
	g, err := b.groupCalls()
	if err != nil {
		return err
	}
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(g.EndGroupCall(ctx, groupID, room, callID))
}

func (b *v2CallBackend) EndGroupCall(groupID, callID string) error { return b.end(groupID, "", callID) }

func (b *v2CallBackend) EndRoomCall(groupID, room, callID string) error {
	return b.end(groupID, room, callID)
}

func (b *v2CallBackend) LeaveRoomCall(groupID, room, callID string) error {
	g, err := b.groupCalls()
	if err != nil {
		return err
	}
	ctx, cancel := v2Ctx()
	defer cancel()
	return v2Err(g.LeaveRoomCall(ctx, groupID, room, callID))
}

// InviteToCall invites a contact to a call: a message of the chat that carries the secret of the call and
// who coordinates it, the invitation of v1 (the chat shows it as a call, convlog.CallInvitePrefix).
func (b *v2CallBackend) InviteToCall(pubKeyHex string) (CallInvite, error) {
	g, err := b.groupCalls()
	if err != nil {
		return CallInvite{}, err
	}
	ctx, cancel := v2Ctx()
	defer cancel()
	id := call.NewInviteSecret()
	coord := b.calls.Self().String()
	text := convlog.CallInvitePrefix + id + "/" + coord
	var m convlog.Message
	err = asContact(pubKeyHex, func(node string) (err error) {
		m, err = g.ChatSend(ctx, app.ChatRef{Kind: app.KindDM, ID: node}, app.SendReq{Text: text})
		return err
	})
	if err != nil && m.ID == "" {
		return CallInvite{}, v2Err(err)
	}
	return CallInvite{CallID: id, Coordinator: coord, MessageID: m.ID, State: m.State}, v2Err(err)
}
