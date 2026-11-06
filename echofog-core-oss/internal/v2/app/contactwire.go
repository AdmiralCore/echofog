package app

import (
	"context"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	apitypes "github.com/echofog/echofog-core/pkg/api/types"
)

// Connected implements the v1 contacts.Wire port for v2: is there a live session with the node.
func (a *App) Connected(id apitypes.PeerID) bool { return false }

// Knock asks a contact for its current card (chat/2 Control.knock).
func (a *App) Knock(ctx context.Context, node string) error { return nil }

// OnControl handles a chat/2 control message from an authenticated account. Wire it to
// chat.Events.Control.
func (a *App) OnControl(from [32]byte, c *chatpb.Control) {}
