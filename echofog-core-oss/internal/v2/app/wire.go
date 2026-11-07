package app

import (
	"context"

	"github.com/echofog/echofog-core/internal/v2/chat"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
)

// Receiver is the receiving side of the conversation engine (*convsync.Engine).
type Receiver interface {
	Receive(ctx context.Context, from [32]byte, pm *chatpb.PersonalMessageProto) bool
}

// OnIncoming takes every chat/2 message the runtime delivers (T11.3d, stub).
func (a *App) OnIncoming(ctx context.Context, from [32]byte, pm *chatpb.PersonalMessageProto) bool {
	if r, ok := a.cfg.Convs.(Receiver); ok && r.Receive(ctx, from, pm) {
		return true
	}
	return a.OnGroupMessage(ctx, from, pm)
}

// ChatEvents is what the runtime hands to chat.ServiceConfig.Events (stub).
func (a *App) ChatEvents(st *ChatStates) chat.Events {
	return chat.Events{
		Message: func(chat.Incoming) {},
		Status:  func([16]byte, chat.State) {},
		Control: func([32]byte, *chatpb.Control) {},
	}
}
