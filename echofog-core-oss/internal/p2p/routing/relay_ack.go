package routing

import (
	"encoding/hex"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// A relay answers every message request (TypeRelayMessageRequest) with a TypeRelayAck: it kept the
// message for its recipient, passed it on, or refused it, and why. Before, the sender learnt only a
// full mailbox; every other refusal was silent, and the sender took a request written to the
// relay's link for a message taken, retrying it for a week. An older relay sends nothing; its
// senders keep retrying as before.
const (
	RelayAckStored    byte = 1 // in the recipient's mailbox here
	RelayAckForwarded byte = 2 // handed to the recipient, or to the next relay
	RelayAckRefused   byte = 3 // not taken: see the reason
	// RelayAckReceived goes the other way: a recipient tells the relay that handed it a message
	// that it arrived (messenger.MailboxReceipt).
	RelayAckReceived = messenger.MailboxReceipt
)

// Why a relay refused a message.
const (
	RefusedNoOrigin byte = 1 // the request named no signed origin
	RefusedNotRelay byte = 2 // this node relays nothing (signaling only, or relaying off)
	RefusedPrivate  byte = 3 // a private relay that does not serve the sender
	RefusedRate     byte = 4 // the origin sends faster than this relay takes
	RefusedMailbox  byte = 5 // the mailbox would not keep it (full, unknown recipient, none)
)

// maxRelayAckID bounds the request id an acknowledgement carries (ours are 16 bytes).
const maxRelayAckID = 64

func encodeRelayAck(reqID []byte, status, reason byte) []byte {
	return append([]byte{status, reason}, reqID...)
}

// DecodeRelayAck reads a TypeRelayAck frame.
func DecodeRelayAck(b []byte) (reqID []byte, status, reason byte, ok bool) {
	if len(b) < 3 || len(b) > 2+maxRelayAckID || b[0] < RelayAckStored || b[0] > RelayAckRefused {
		return nil, 0, 0, false
	}
	return b[2:], b[0], b[1], true
}

// decodeReceipt reads a recipient's receipt: a TypeRelayAck of status RelayAckReceived.
func decodeReceipt(b []byte) (msgID []byte, ok bool) {
	if len(b) < 3 || len(b) > 2+maxRelayAckID || b[0] != RelayAckReceived {
		return nil, false
	}
	return b[2:], true
}

// maxReceipting bounds the table of recipients known to send receipts.
const maxReceipting = 100000

// handleReceipt drops the mail a recipient says it got, and notes that it sends receipts: its
// other mail is then kept until it does (see finishStored).
func (rs *RelayServer) handleReceipt(payload []byte, from types.PeerID) {
	id, ok := decodeReceipt(payload)
	if !ok || rs.mailbox == nil {
		return
	}
	rs.peersMu.Lock()
	if rs.receipting == nil || len(rs.receipting) >= maxReceipting {
		rs.receipting = map[types.PeerID]bool{}
	}
	rs.receipting[from] = true
	rs.peersMu.Unlock()
	rs.receipts.Add(1)
	rs.mailbox.Receipt(from, hex.EncodeToString(id))
}

// sendsReceipts: the peer has told us of mail it got, so mail it was handed and did not confirm
// is kept for it.
func (rs *RelayServer) sendsReceipts(peer types.PeerID) bool {
	rs.peersMu.Lock()
	defer rs.peersMu.Unlock()
	return rs.receipting[peer]
}

// ack tells the peer that sent a relay request what became of it.
func (rs *RelayServer) ack(to types.PeerID, reqID []byte, status, reason byte) {
	if rs.swarm == nil || len(reqID) == 0 || len(reqID) > maxRelayAckID || !rs.ackReply.Allow(to, 1) {
		return
	}
	if p := rs.swarm.GetPeer(to); p != nil {
		_ = p.SendRaw(net_types.TypeRelayAck, encodeRelayAck(reqID, status, reason))
	}
}
