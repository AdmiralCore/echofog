package convsync

import (
	"context"

	"github.com/echofog/echofog-core/internal/v2/chat"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
)

// ChatNet is the Net over a chat/2 service: every log message is an ordinary end-to-end message, so
// it gets the outbox, the mailbox and the statuses of chat/2.
type ChatNet struct{ Svc *chat.Service }

// Send queues the message for the account behind node.
func (n ChatNet) Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) error {
	_, err := n.Svc.Send(ctx, to, m)
	return err
}
