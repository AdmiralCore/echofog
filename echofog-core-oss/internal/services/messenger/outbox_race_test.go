package messenger

import (
	"testing"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// The receipt can come while the outbox is sending the very message again (the recipient got
// an earlier copy). The confirmed message must stay gone: the flush must not write it back.
func TestReceiptDuringFlushIsNotUndone(t *testing.T) {
	var s *MessageBroker
	var peer types.PeerID
	peer[0] = 7
	s = outboxBroker(t, func(types.PeerID, *internal_pb.MessageData) (bool, bool) {
		s.confirm(peer, "m1") // the receipt arrives meanwhile
		return true, true
	})
	if err := s.trackVia(peer, "m1", &internal_pb.MessageData{MessageId: []byte{1}}, []byte("inner"), false, false); err != nil {
		t.Fatal(err)
	}
	s.FlushOutbox(peer)
	if st := s.PendingStates(peer); len(st) != 0 {
		t.Fatalf("a confirmed message is back in the outbox: %v", st)
	}
}
