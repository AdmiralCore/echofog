package messenger

import (
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// What a relay answered about a message we handed it (routing.RelayAck*). A relay that kept the
// message for its recipient, or passed it on, is trusted with it: the message goes through relays
// again only every relayKeptRetry, in case the relay lost it (a full mailbox drops its oldest). A
// refusal puts the message back to waiting for a way, and it is tried again soon, through another
// relay (the refusing one goes last for a while). A relay of an older version answers nothing; its
// messages keep the hourly retry (relayedBackoff). In every case a message still goes at once
// when the recipient connects to us.
const (
	relayKeptRetry = 12 * time.Hour
	relayWaitMax   = 4096            // relay answers awaited at once
	relayWaitTTL   = 2 * time.Minute // an answer later than this is not waited for
)

// MailboxReceipt is the TypeRelayAck status a recipient sends to the relay that handed it a
// message: [MailboxReceipt][0][message id]. A relay that hears receipts from a recipient keeps
// its mail until the receipt (or the expiry) instead of dropping it after handing it over twice.
// A relay of an older version does not know the type and drops the frame, as older senders drop
// relay acknowledgements.
const MailboxReceipt byte = 4

// maxReceiptID bounds the message id a receipt carries (ours are 16 bytes).
const maxReceiptID = 64

// sendReceipt tells via, which handed us a message that origin signed, that it arrived.
func (s *MessageBroker) sendReceipt(via, origin types.PeerID, msgID []byte) {
	if s.sw == nil || via == origin || len(msgID) == 0 || len(msgID) > maxReceiptID {
		return
	}
	if p := s.sw.GetPeer(via); p != nil {
		_ = p.SendRaw(net_types.TypeRelayAck, append([]byte{MailboxReceipt, 0}, msgID...))
	}
}

type relayWait struct {
	target types.PeerID
	id     string
	at     time.Time
}

// awaitRelay notes that the relay request reqID carries outbox message id to target.
func (s *MessageBroker) awaitRelay(reqID []byte, target types.PeerID, id string) {
	now := time.Now()
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	if s.relayWait == nil {
		s.relayWait = make(map[string]relayWait)
	}
	if len(s.relayWait) >= relayWaitMax {
		for k, w := range s.relayWait {
			if now.Sub(w.at) > relayWaitTTL {
				delete(s.relayWait, k)
			}
		}
		if len(s.relayWait) >= relayWaitMax {
			return // that many unanswered: this one falls back to the hourly retry
		}
	}
	s.relayWait[hex.EncodeToString(reqID)] = relayWait{target: target, id: id, at: now}
}

// HandleRelayAck takes a relay's answer to one of our message requests: taken (kept or passed on)
// or refused. An answer to a request we did not make, or no longer wait for, changes nothing.
func (s *MessageBroker) HandleRelayAck(relay types.PeerID, reqID []byte, taken bool) {
	key := hex.EncodeToString(reqID)
	s.relayMu.Lock()
	w, ok := s.relayWait[key]
	delete(s.relayWait, key)
	s.relayMu.Unlock()
	if !ok || time.Since(w.at) > relayWaitTTL || s.kv == nil {
		return
	}
	now := time.Now()
	s.outboxMu.Lock() // after the send's own write (see FlushOutbox)
	defer s.outboxMu.Unlock()
	for _, e := range s.outboxEntries(outboxFolder(s.kv, w.target)) {
		var it outboxItem
		if json.Unmarshal(e.Value, &it) != nil || it.ID != w.id {
			continue
		}
		if taken {
			if !it.Relayed {
				return // the recipient took it directly meanwhile
			}
			it.RelayKept = true
			it.Next = now.Add(relayKeptRetry).UnixNano()
			s.log.Info("MessageBroker: a relay keeps the message for the peer", "peer", w.target.String()[:8], "relay", relay.String()[:8])
		} else {
			it.RelayKept = false
			if it.Relayed {
				it.Sent, it.Relayed = false, false
				s.noteState(w.target, it.ID, it.state())
			}
			it.Next = now.Add(backoff(it.Attempts)).UnixNano()
			s.log.Info("MessageBroker: a relay refused the message, trying another way", "peer", w.target.String()[:8], "relay", relay.String()[:8])
		}
		s.writeLocked(e.Key, it)
		return
	}
}
