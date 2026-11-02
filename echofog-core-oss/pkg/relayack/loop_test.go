package relayack

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/routing"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	coreNode "github.com/echofog/echofog-core/pkg/node"
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
			TargetPeerId: nobody[:], ForwardedEnvelope: signedEnvelope(t, aN),
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

// A relay that does not keep mail for the recipient and knows none of the relays the recipient
// named for its mail does not hand the request to some other relay: it refuses it, and the sender
// tries another way. Before, the request wandered from relay to relay.
func TestRelayRequestDoesNotWander(t *testing.T) {
	aN, _ := start(t, "sender")
	r1, _ := start(t, "relay-1")
	r2, _ := start(t, "relay-2")
	connect(t, r1, r2)
	connect(t, aN, r1)

	var nobody types.PeerID
	nobody[0], nobody[31] = 0xbe, 0xef
	self := aN.Swarm.GetSelfID()
	req := &internal_pb.MessageData{
		MessageId: types.NewMessageID(),
		OriginId:  self.ToBytes(),
		TargetId:  nobody[:],
		Timestamp: uint64(time.Now().UnixNano()),
		HopLimit:  types.HOPLIMIT,
		Payload: &internal_pb.MessageData_RelayReq{RelayReq: &internal_pb.RelayRequest{
			TargetPeerId: nobody[:], ForwardedEnvelope: signedEnvelope(t, aN),
		}},
	}
	env, err := aN.Swarm.SignMessageData(req)
	require.NoError(t, err)

	statuses := make(chan byte, 4)
	aN.RelayClient.OnRelayAck(func(_ types.PeerID, _ []byte, status, _ byte) { statuses <- status })
	require.NoError(t, aN.Swarm.GetPeer(r1.ID).Send(net_types.TypeRelayMessageRequest, env))

	select {
	case st := <-statuses:
		require.Equal(t, routing.RelayAckRefused, st, "the relay took a request it can only pass on at random")
	case <-time.After(5 * time.Second):
		t.Fatal("no answer from the relay")
	}
	time.Sleep(500 * time.Millisecond)
	require.Zero(t, r1.RelayServer.Stats().Forwarded, "the request was handed to a relay that does not keep the recipient's mail")
	require.Zero(t, r2.RelayServer.Stats().Stored+r2.RelayServer.Stats().Forwarded)
}

// signedEnvelope is a message signed by n, as a relay request carries it.
func signedEnvelope(t *testing.T, n *coreNode.Node) []byte {
	env, err := n.Swarm.SignMessageData(&internal_pb.MessageData{MessageId: types.NewMessageID(), OriginId: n.ID.ToBytes(),
		Timestamp: uint64(time.Now().Unix())})
	require.NoError(t, err)
	b, err := proto.Marshal(env)
	require.NoError(t, err)
	return b
}
