package relayack

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// Three relays that all know each other, and one request whose sender set a huge hop limit, for a
// recipient nobody can reach: it must not go round the triangle. Each relay passes a request on at
// most once, and never further than the fixed hop limit, whatever the sender wrote.
func TestRelayRequestDoesNotLoop(t *testing.T) {
	aN, _ := start(t, "sender")
	r1, _ := start(t, "relay-1")
	r2, _ := start(t, "relay-2")
	r3, _ := start(t, "relay-3")
	connect(t, r1, r2)
	connect(t, r2, r3)
	connect(t, r3, r1)
	connect(t, aN, r1)

	var nobody types.PeerID
	nobody[0], nobody[31] = 0xde, 0xad
	self := aN.Swarm.GetSelfID()
	req := &internal_pb.MessageData{
		MessageId: types.NewMessageID(),
		OriginId:  self.ToBytes(),
		TargetId:  nobody[:],
		Timestamp: uint64(time.Now().UnixNano()),
		HopLimit:  4_000_000_000,
		Payload: &internal_pb.MessageData_RelayReq{RelayReq: &internal_pb.RelayRequest{
			TargetPeerId: nobody[:], ForwardedEnvelope: mustEnvelope(t),
		}},
	}
	env, err := aN.Swarm.SignMessageData(req)
	require.NoError(t, err)
	require.NoError(t, aN.Swarm.GetPeer(r1.ID).Send(net_types.TypeRelayMessageRequest, env))

	time.Sleep(3 * time.Second) // long enough for a loop to go round many times
	total := r1.RelayServer.Stats().Forwarded + r2.RelayServer.Stats().Forwarded + r3.RelayServer.Stats().Forwarded
	t.Logf("forwards of one request: %d", total)
	require.LessOrEqual(t, total, uint64(3), "the request went round the relays")
}

func mustEnvelope(t *testing.T) []byte {
	b, err := proto.Marshal(&internal_pb.Envelope{Data: []byte("x")})
	require.NoError(t, err)
	return b
}
