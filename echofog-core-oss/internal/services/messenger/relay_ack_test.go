package messenger

import (
	"encoding/json"
	"testing"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// relayAnswering is an outbox whose every send goes to a relay that answers taken or refused, as
// the relay's acknowledgement would arrive over the network.
func relayAnswering(t *testing.T, taken bool) (*MessageBroker, *int) {
	sends := 0
	var s *MessageBroker
	s = outboxBroker(t, func(target types.PeerID, id string, _ *internal_pb.MessageData) (bool, bool) {
		sends++
		req := []byte{byte(sends), 0xac}
		s.awaitRelay(req, target, id)
		go s.HandleRelayAck(types.PeerID{0x52}, req, taken)
		return true, false
	})
	return s, &sends
}

func waitItem(t *testing.T, s *MessageBroker, peer types.PeerID, cond func(outboxItem) bool) outboxItem {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, it := outboxItemOf(t, s, peer)
		if cond(it) {
			return it
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay's answer did not reach the message: %+v", it)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A relay that answers it keeps the message is trusted with it: over a week the message goes
// through relays again only as a rare fallback, not hourly.
func TestRelayThatKeepsTheMessageIsTrusted(t *testing.T) {
	s, sends := relayAnswering(t, true)
	var peer types.PeerID
	peer[0] = 3
	if err := s.trackVia(peer, "m1", &internal_pb.MessageData{MessageId: []byte{1}}, []byte("inner"), false, false); err != nil {
		t.Fatal(err)
	}
	s.FlushOutbox(types.PeerID{})
	waitItem(t, s, peer, func(it outboxItem) bool { return it.RelayKept })
	if st := s.PendingStates(peer)["m1"]; st != StateRelayed {
		t.Fatalf("state %q, want %q", st, StateRelayed)
	}
	var elapsed time.Duration
	for elapsed < outboxTTL {
		s.FlushOutbox(types.PeerID{})
		if len(s.outboxEntries(outboxFolder(s.kv, peer))) == 0 {
			break // given up after outboxTTL
		}
		it := waitItem(t, s, peer, func(it outboxItem) bool { return it.RelayKept })
		wait := time.Until(time.Unix(0, it.Next))
		if wait < relayKeptRetry-time.Minute {
			t.Fatalf("a kept message is due again in %v", wait)
		}
		elapsed += wait
		key, _ := outboxItemOf(t, s, peer)
		it.Next, it.Created = time.Now().UnixNano(), it.Created-int64(wait)
		raw, _ := json.Marshal(it)
		_ = s.kv.Set(key, raw)
	}
	if *sends > 16 {
		t.Fatalf("%d relay requests in a week for a message a relay keeps", *sends)
	}
}

// A refusal puts the message back to waiting for a way, and it is tried again within seconds.
func TestRelayRefusalIsTriedAgainSoon(t *testing.T) {
	s, _ := relayAnswering(t, false)
	var peer types.PeerID
	peer[0] = 4
	if err := s.trackVia(peer, "m1", &internal_pb.MessageData{MessageId: []byte{1}}, []byte("inner"), false, false); err != nil {
		t.Fatal(err)
	}
	s.FlushOutbox(types.PeerID{})
	it := waitItem(t, s, peer, func(it outboxItem) bool { return !it.Relayed })
	if it.Sent || it.RelayKept {
		t.Fatalf("a refused message counts as taken: %+v", it)
	}
	if wait := time.Until(time.Unix(0, it.Next)); wait > time.Minute {
		t.Fatalf("a refused message waits %v before the next try", wait)
	}
	if st := s.PendingStates(peer)["m1"]; st != StateOffline {
		t.Fatalf("state %q, want %q", st, StateOffline)
	}
}

// An answer nobody waits for (another request, or a forged one) changes nothing.
func TestUnknownRelayAnswerIsIgnored(t *testing.T) {
	s := outboxBroker(t, func(types.PeerID, string, *internal_pb.MessageData) (bool, bool) { return true, false })
	var peer types.PeerID
	peer[0] = 5
	if err := s.trackVia(peer, "m1", &internal_pb.MessageData{MessageId: []byte{1}}, []byte("inner"), true, true); err != nil {
		t.Fatal(err)
	}
	s.HandleRelayAck(types.PeerID{1}, []byte{9, 9}, false)
	if _, it := outboxItemOf(t, s, peer); !it.Relayed || !it.Sent {
		t.Fatalf("an unknown answer changed the message: %+v", it)
	}
}
