package routing

import (
	"crypto/ed25519"
	"testing"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// A relay request counts against the signer of the message it carries, not against the relay
// that passed it on: the mailbox item and the rate limit are keyed by that sender.
func TestRelayRequestCountsAgainstTheRealSender(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	sender := types.PeerPubKeyToID(types.PeerPublicKey(pub))
	data := []byte("message data")
	fwd, _ := proto.Marshal(&internal_pb.Envelope{PubKey: pub, Data: data, Signature: ed25519.Sign(priv, data)})

	got, env, ok := requestSender(fwd)
	if !ok || got != sender || env == nil {
		t.Fatalf("sender = %v ok=%v, want the signer", got, ok)
	}
	relay := mkID(0x77) // the relay that passed the request on and signed it
	item := mailboxItemOf(&internal_pb.MessageData{MessageId: []byte{1}}, mkID(0x0b), got, fwd)
	if item.SenderID != sender || item.SenderID == relay {
		t.Fatalf("mailbox item counts against %v, want the signer", item.SenderID)
	}

	bad, _ := proto.Marshal(&internal_pb.Envelope{PubKey: pub, Data: []byte("other"), Signature: ed25519.Sign(priv, data)})
	if _, _, ok := requestSender(bad); ok {
		t.Fatal("a message with a wrong signature has a sender")
	}
	if _, _, ok := requestSender([]byte("not an envelope")); ok {
		t.Fatal("garbage has a sender")
	}
}
