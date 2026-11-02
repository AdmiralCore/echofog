package messenger

import (
	"testing"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// The receipt can come while the outbox is sending the very message again (the recipient got
// an earlier copy). The confirmed message must stay gone: the flush must not write it back.
func TestReceiptDuringFlushIsNotUndone(t *testing.T) {
	var s *MessageBroker
	var peer types.PeerID
	peer[0] = 7
	confirmed := make(chan struct{})
	s = outboxBroker(t, func(types.PeerID, string, *internal_pb.MessageData) (bool, bool) {
		go func() { s.confirm(peer, "m1"); close(confirmed) }() // the receipt arrives meanwhile
		time.Sleep(20 * time.Millisecond)
		return true, true
	})
	if err := s.trackVia(peer, "m1", &internal_pb.MessageData{MessageId: []byte{1}}, []byte("inner"), false, false); err != nil {
		t.Fatal(err)
	}
	s.FlushOutbox(peer)
	<-confirmed
	if st := s.PendingStates(peer); len(st) != 0 {
		t.Fatalf("a confirmed message is back in the outbox: %v", st)
	}
}
