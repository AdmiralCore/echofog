package messenger

import (
	"fmt"
	"testing"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// A long queue for a peer only reachable through a relay goes out in parts: a relay drops what
// one sender hands it over its burst (60) without telling, so a flush must stay under it.
func TestFlushStaysUnderTheRelayBurst(t *testing.T) {
	sends := 0
	s := outboxBroker(t, func(types.PeerID, string, *internal_pb.MessageData) (bool, bool) {
		sends++
		return true, false
	})
	var peer types.PeerID
	peer[0] = 5
	for i := 0; i < 100; i++ {
		if err := s.trackVia(peer, fmt.Sprint("m", i), &internal_pb.MessageData{MessageId: []byte{byte(i)}}, []byte("inner"), false, false); err != nil {
			t.Fatal(err)
		}
	}
	s.FlushOutbox(types.PeerID{})
	if sends == 0 || sends > relayPerFlush {
		t.Fatalf("one flush handed %d messages to relays, want 1..%d", sends, relayPerFlush)
	}
	first := sends
	s.FlushOutbox(types.PeerID{})
	if sends <= first {
		t.Fatalf("the rest did not go at the next flush (%d then %d)", first, sends)
	}
}
