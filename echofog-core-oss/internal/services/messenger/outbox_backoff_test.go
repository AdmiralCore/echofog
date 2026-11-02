package messenger

import (
	"encoding/json"
	"testing"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// A message a relay took (as the sender sees it: the frame went out on the relay's link) and
// whose receipt never comes — the recipient is gone for good, or the relay dropped the request
// without a word — is sent again on a virtual clock until the outbox gives up on it. It must not
// cost a relay request a minute for a week.
func TestRelayedMessageWithoutReceiptBacksOff(t *testing.T) {
	sends := 0
	s := outboxBroker(t, func(types.PeerID, *internal_pb.MessageData) (bool, bool) {
		sends++
		return true, false // a relay took it
	})
	var peer types.PeerID
	peer[0] = 9
	if err := s.trackVia(peer, "m1", &internal_pb.MessageData{MessageId: []byte{1}}, []byte("inner"), true, true); err != nil {
		t.Fatal(err)
	}
	// virtual time: each round jumps to the moment the item is due
	var elapsed time.Duration
	for elapsed < outboxTTL+time.Hour {
		entries := s.outboxEntries(outboxFolder(s.kv, peer))
		if len(entries) == 0 {
			break
		}
		key, it := outboxItemOf(t, s, peer)
		wait := time.Until(time.Unix(0, it.Next))
		if wait < 0 {
			wait = 0
		}
		elapsed += wait
		it.Next = time.Now().UnixNano()
		it.Created -= int64(wait)
		raw, _ := json.Marshal(it)
		_ = s.kv.Set(key, raw)
		s.FlushOutbox(types.PeerID{})
	}
	t.Logf("one relayed message without a receipt: %d sends in %v", sends, elapsed.Round(time.Hour))
	if sends > 7*24+20 {
		t.Fatalf("%d relay requests for one message in a week (at most about one an hour wanted)", sends)
	}
}
