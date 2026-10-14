package messenger

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	gproto "google.golang.org/protobuf/proto"
)

// The outbox keeps every message until the recipient confirms it. A message goes out at once, and
// stays here until the recipient's node sends back a receipt ("fogack1:<id>", end-to-end encrypted
// like everything else). If the recipient is offline, not ready yet, or the packet was lost, the
// message is sent again with growing pauses, and when the recipient (or a relay) connects. After
// outboxTTL it is dropped. What is stored is the finished ciphertext, so a retry changes nothing
// about the encryption state, and the recipient drops duplicates by message id.
const (
	outboxPrefix   = "outbox/"
	seenPrefix     = "seen/"
	ackPrefix      = "fogack1:"
	outboxTTL      = 7 * 24 * time.Hour
	outboxInterval = 3 * time.Second
	outboxMaxItems = 5000
)

type outboxItem struct {
	ID       string       `json:"id"` // the inner message id, shown as "pending" in the chat
	Target   types.PeerID `json:"target"`
	Data     []byte       `json:"data,omitempty"`   // marshalled MessageData
	Intent   []byte       `json:"intent,omitempty"` // not encrypted yet: the inner message, waiting for keys
	Created  int64        `json:"created"`
	Attempts int          `json:"attempts"`
	Next     int64        `json:"next"` // unix nano: do not retry before
}

// outboxKey sorts by time, so that what was written first is sent first (group changes depend on
// their order).
func outboxKey(folder string, created int64, id string) []byte {
	return []byte(fmt.Sprintf("%s%020d-%s", folder, created, id))
}

// outboxFolder is the key prefix of what waits for target; the target is blinded in it.
func outboxFolder(kv storage.Storage, target types.PeerID) string {
	return outboxPrefix + storage.BlindID(kv, hex.EncodeToString(target[:])) + "/"
}

// backoff is the pause after the n-th attempt: 2 s, 5 s, 15 s, 45 s, then every minute.
func backoff(n int) time.Duration {
	steps := []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 45 * time.Second}
	if n < len(steps) {
		return steps[n]
	}
	return time.Minute
}

// track keeps msg until target confirms it. delivered says the first attempt already went out.
func (s *MessageBroker) track(target types.PeerID, id string, msg *internal_pb.MessageData, inner []byte, delivered bool) error {
	data, err := gproto.Marshal(msg)
	if err != nil {
		return err
	}
	if s.kv == nil {
		return fmt.Errorf("no storage for the outbox")
	}
	if all, _ := s.kv.FindValues([]byte(outboxPrefix), outboxMaxItems+1); len(all) > outboxMaxItems {
		return fmt.Errorf("the outbox is full")
	}
	now := time.Now()
	it := outboxItem{ID: id, Target: target, Data: data, Intent: inner, Created: now.UnixNano(), Next: now.UnixNano()}
	if delivered {
		it.Attempts = 1
		it.Next = now.Add(backoff(0)).UnixNano()
	}
	raw, err := json.Marshal(it)
	if err != nil {
		return err
	}
	if !delivered {
		s.log.Info("MessageBroker: queued a message for later", "target", target.String()[:8])
	}
	return s.kv.Set(outboxKey(outboxFolder(s.kv, target), it.Created, id), raw)
}

// trackIntent keeps a message that could not be encrypted yet (the keys of the peer are unknown).
// It is encrypted and sent when the peer shows up.
func (s *MessageBroker) trackIntent(target types.PeerID, id string, inner []byte) error {
	if s.kv == nil {
		return fmt.Errorf("no storage for the outbox")
	}
	if all, _ := s.kv.FindValues([]byte(outboxPrefix), outboxMaxItems+1); len(all) > outboxMaxItems {
		return fmt.Errorf("the outbox is full")
	}
	now := time.Now().UnixNano()
	raw, err := json.Marshal(outboxItem{ID: id, Target: target, Intent: inner, Created: now, Next: now})
	if err != nil {
		return err
	}
	s.log.Info("MessageBroker: no keys for the peer yet, message kept", "target", target.String()[:8])
	return s.kv.Set(outboxKey(outboxFolder(s.kv, target), now, id), raw)
}

func (s *MessageBroker) outboxEntries(prefix string) []storage.Entry {
	if s.kv == nil {
		return nil
	}
	entries, _ := s.kv.FindValues([]byte(prefix), outboxMaxItems)
	return entries
}

// PendingIDs are the ids of the messages to target that the recipient has not confirmed yet.
func (s *MessageBroker) PendingIDs(target types.PeerID) map[string]bool {
	out := make(map[string]bool)
	for _, e := range s.outboxEntries(outboxFolder(s.kv, target)) {
		var it outboxItem
		if json.Unmarshal(e.Value, &it) == nil {
			out[it.ID] = true
		}
	}
	return out
}

// forgetCiphertexts drops the encrypted form of everything waiting for target: the session it was
// encrypted with is gone, so each message is encrypted again, with the new session, when it is sent.
func (s *MessageBroker) forgetCiphertexts(target types.PeerID) {
	for _, e := range s.outboxEntries(outboxFolder(s.kv, target)) {
		var it outboxItem
		if json.Unmarshal(e.Value, &it) != nil || len(it.Intent) == 0 {
			continue
		}
		it.Data = nil
		it.Next = time.Now().UnixNano()
		if raw, err := json.Marshal(it); err == nil {
			_ = s.kv.Set(e.Key, raw)
		}
	}
}

// confirm removes the message that target acknowledged.
func (s *MessageBroker) confirm(target types.PeerID, id string) {
	for _, e := range s.outboxEntries(outboxFolder(s.kv, target)) {
		var it outboxItem
		if json.Unmarshal(e.Value, &it) == nil && it.ID == id {
			_ = s.kv.Delete(e.Key)
			return
		}
	}
}

// FlushOutbox sends again what waits for target, ignoring the pauses (everybody's due messages
// when target is the zero ID), and returns how many messages went out.
func (s *MessageBroker) FlushOutbox(target types.PeerID) int {
	prefix, force := outboxPrefix, false
	if target != (types.PeerID{}) {
		prefix = outboxFolder(s.kv, target)
		force = true
	}
	sent := 0
	now := time.Now()
	for _, e := range s.outboxEntries(prefix) {
		var it outboxItem
		if json.Unmarshal(e.Value, &it) != nil {
			_ = s.kv.Delete(e.Key)
			continue
		}
		if now.Sub(time.Unix(0, it.Created)) > outboxTTL {
			_ = s.kv.Delete(e.Key)
			continue
		}
		if !force && it.Next > now.UnixNano() {
			continue
		}
		if len(it.Data) == 0 {
			// not encrypted yet: possible once the peer's keys are known
			enc, err := s.encryptX3DH(it.Target, it.Intent)
			if err != nil {
				s.wantConnection(it.Target)
				it.Next = now.Add(backoff(it.Attempts)).UnixNano()
				it.Attempts++
				if raw, merr := json.Marshal(it); merr == nil {
					_ = s.kv.Set(e.Key, raw)
				}
				continue
			}
			data, merr := gproto.Marshal(enc)
			if merr != nil {
				_ = s.kv.Delete(e.Key)
				continue
			}
			it.Data = data // the intent stays: a session reset needs it
		}
		var msg internal_pb.MessageData
		if gproto.Unmarshal(it.Data, &msg) != nil {
			_ = s.kv.Delete(e.Key)
			continue
		}
		if s.deliver(it.Target, &msg) {
			sent++
		} else {
			s.wantConnection(it.Target)
		}
		it.Attempts++
		it.Next = now.Add(backoff(it.Attempts - 1)).UnixNano()
		if raw, err := json.Marshal(it); err == nil {
			_ = s.kv.Set(e.Key, raw)
		}
	}
	if sent > 0 {
		s.log.Info("MessageBroker: sent queued messages", "count", sent)
	}
	return sent
}

// StartOutbox retries the outbox until ctx ends and forgets old message ids.
func (s *MessageBroker) StartOutbox(ctx context.Context) {
	go func() {
		t := time.NewTicker(outboxInterval)
		defer t.Stop()
		pruned := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.FlushOutbox(types.PeerID{})
				if time.Since(pruned) > time.Hour {
					s.pruneSeen()
					pruned = time.Now()
				}
			}
		}
	}()
}

// firstSight records a message id and reports whether it is new. Duplicates arrive when a receipt
// got lost and the sender tried again.
func (s *MessageBroker) firstSight(id string) bool {
	if s.kv == nil || id == "" {
		return true
	}
	key := []byte(seenPrefix + id)
	if s.kv.Exists(key) {
		return false
	}
	_ = s.kv.Set(key, []byte(time.Now().Format(time.RFC3339)))
	return true
}

func (s *MessageBroker) pruneSeen() {
	if s.kv == nil {
		return
	}
	entries, _ := s.kv.FindValues([]byte(seenPrefix), 100000)
	for _, e := range entries {
		t, err := time.Parse(time.RFC3339, string(e.Value))
		if err != nil || time.Since(t) > outboxTTL+24*time.Hour {
			_ = s.kv.Delete(e.Key)
		}
	}
}

// sendAck tells origin that message id arrived. Receipts are not tracked themselves.
func (s *MessageBroker) sendAck(origin types.PeerID, id string) {
	_ = s.SendChatMessageWithTier(origin, ackPrefix+id, TierX3DH, SendOptions{Silent: true, noTrack: true})
}

// OnPeerConnected sends what waited for this peer. It makes the broker a swarm topology observer.
func (s *MessageBroker) OnPeerConnected(peerID types.PeerID, _ types.PeerPublicKey, _ string) {
	s.FlushOutbox(peerID)
}

func (s *MessageBroker) OnPeerDisconnected(types.PeerID) {}
