package routing

import (
	"encoding/hex"
	"testing"

	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"google.golang.org/protobuf/proto"
)

// A recipient's receipt drops the item kept for it under the message's id, and marks the
// recipient as one that sends receipts (its unconfirmed mail is then kept). A receipt from
// somebody else drops nothing; the relay client leaves receipts to the relay server.
func TestRelayServer_ReceiptDropsTheMail(t *testing.T) {
	mb := messenger.NewMailboxManager(newTestStore())
	rs := NewRelayServer(nil, mb, nil)
	zed, other := mkID(0x0b), mkID(0x0c)
	mb.NoteRecipientSeen(zed)

	msgID := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	data, _ := proto.Marshal(&internal_pb.MessageData{MessageId: msgID})
	env, _ := proto.Marshal(&internal_pb.Envelope{Data: data})
	item := mailboxItemOf(&internal_pb.MessageData{MessageId: []byte{9}}, zed, other, env)
	if item.MsgID != hex.EncodeToString(msgID) {
		t.Fatalf("item names message %q, want the inner one", item.MsgID)
	}
	if err := mb.Store(item); err != nil {
		t.Fatal(err)
	}

	receipt := append([]byte{RelayAckReceived, 0}, msgID...)
	if NewRelayClient(nil, nil, nil).HandleRawFrame(net_types.TypeRelayAck, receipt, zed) {
		t.Fatal("the relay client took a receipt")
	}
	if !rs.HandleRawFrame(net_types.TypeRelayAck, receipt, other) || mb.Count(zed) != 1 {
		t.Fatal("somebody else's receipt dropped the mail")
	}
	rs.HandleRawFrame(net_types.TypeRelayAck, receipt, zed)
	if mb.Count(zed) != 0 {
		t.Fatal("the recipient's receipt did not drop the mail")
	}
	if !rs.sendsReceipts(zed) {
		t.Fatal("the recipient is not known to send receipts")
	}
}
