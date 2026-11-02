package routing

import (
	"bytes"
	"testing"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"google.golang.org/protobuf/proto"
)

func TestRelayAckRoundTrip(t *testing.T) {
	id := bytes.Repeat([]byte{7}, 16)
	got, status, reason, ok := DecodeRelayAck(encodeRelayAck(id, RelayAckRefused, RefusedPrivate))
	if !ok || !bytes.Equal(got, id) || status != RelayAckRefused || reason != RefusedPrivate {
		t.Fatalf("round trip: %x %d %d %v", got, status, reason, ok)
	}
	for _, bad := range [][]byte{nil, {1, 0}, {9, 0, 1}, append([]byte{1, 0}, make([]byte, maxRelayAckID+1)...)} {
		if _, _, _, ok := DecodeRelayAck(bad); ok {
			t.Fatalf("accepted %x", bad)
		}
	}
}

// A node of an older version does not know TypeRelayAck: its swarm takes the frame for an envelope
// and its dispatcher drops any envelope without a 32-byte key, silently and without blaming the
// sender. Every possible answer ends there, so an older sender ignores what a new relay says: the
// first byte (status 1-3) reads as a protobuf tag of field number 0, which no decoder accepts.
func TestOlderNodesDropRelayAcks(t *testing.T) {
	for status := RelayAckStored; status <= RelayAckRefused; status++ {
		for reason := byte(0); reason <= RefusedMailbox; reason++ {
			for _, id := range [][]byte{bytes.Repeat([]byte{0x0a}, 16), bytes.Repeat([]byte{0x22}, 16), make([]byte, 16)} {
				var env internal_pb.Envelope
				if proto.Unmarshal(encodeRelayAck(id, status, reason), &env) == nil && len(env.PubKey) == 32 {
					t.Fatalf("an older node would read answer %d/%d as an envelope with a key", status, reason)
				}
			}
		}
	}
}
