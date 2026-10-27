package swarm

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Mailbox rendezvous: a node names the relays that keep its mail while it is away (its mailbox
// relays) in its key bundle record, signed with its identity key. A sender that cannot reach the
// node hands its message to one of them, instead of hoping that one of its own relays happens to
// be one the node uses. See docs/design/conversation-log.md, section 6.

// MailboxHint names one mailbox relay.
type MailboxHint struct {
	Peer  string `json:"p"`           // the relay's peer id, hex
	Addr  string `json:"a,omitempty"` // where it was reached (host:port)
	Until int64  `json:"u"`           // unix seconds; the hint is stale after it
}

const (
	maxMailboxHints = 4
	maxHintAddr     = 256
	maxHintLife     = 7 * 24 * time.Hour
)

func mailboxSigned(hints []MailboxHint) []byte {
	raw, _ := json.Marshal(hints)
	return append([]byte("echofog-mbox-v1"), raw...)
}

func verifyMailboxes(pub types.PeerPublicKey, hints []MailboxHint, sig []byte) bool {
	if len(hints) > maxMailboxHints || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub[:]), mailboxSigned(hints), sig)
}

// SetMailboxHints sets the relays that keep this node's mail; they go into its bundle record and
// identity card from now on.
func (s *Swarm) SetMailboxHints(hints []MailboxHint) {
	if len(hints) > maxMailboxHints {
		hints = hints[:maxMailboxHints]
	}
	s.cardMu.Lock()
	s.mailboxes = append([]MailboxHint(nil), hints...)
	s.card = nil // the next card carries them
	s.cardMu.Unlock()
}

// MyMailboxHints returns this node's mailbox relays.
func (s *Swarm) MyMailboxHints() []MailboxHint {
	s.cardMu.Lock()
	defer s.cardMu.Unlock()
	return append([]MailboxHint(nil), s.mailboxes...)
}

// signedMailboxesLocked returns our mailbox hints and their signature (none when there are no
// hints or no identity). Caller holds cardMu.
func (s *Swarm) signedMailboxesLocked() ([]MailboxHint, []byte) {
	if len(s.mailboxes) == 0 || s.identity == nil {
		return nil, nil
	}
	hints := append([]MailboxHint(nil), s.mailboxes...)
	sig, err := s.identity.SignData(mailboxSigned(hints))
	if err != nil {
		return nil, nil
	}
	return hints, sig
}

func (s *Swarm) mailboxKey(peer types.PeerID) []byte {
	return []byte("mbx:" + storage.BlindID(s.storage, peer.String()))
}

func cleanHints(hints []MailboxHint, now time.Time) []MailboxHint {
	out := make([]MailboxHint, 0, len(hints))
	for _, h := range hints {
		if b, err := hex.DecodeString(h.Peer); err != nil || len(b) != len(types.PeerID{}) {
			continue
		}
		if len(h.Addr) > maxHintAddr || h.Until <= now.Unix() || h.Until > now.Add(maxHintLife).Unix() {
			continue
		}
		out = append(out, h)
		if len(out) == maxMailboxHints {
			break
		}
	}
	return out
}

// rememberMailboxes keeps the mailbox relays a peer's record named.
func (s *Swarm) rememberMailboxes(peer types.PeerID, hints []MailboxHint) {
	hints = cleanHints(hints, time.Now())
	if len(hints) == 0 {
		return
	}
	s.mbxMu.Lock()
	if s.mbx == nil {
		s.mbx = map[types.PeerID][]MailboxHint{}
	}
	s.mbx[peer] = hints
	s.mbxMu.Unlock()
	if raw, err := json.Marshal(hints); err == nil {
		_ = s.storage.Set(s.mailboxKey(peer), raw)
	}
}

// MailboxHintsOf returns the mailbox relays a peer named last, without the stale ones.
func (s *Swarm) MailboxHintsOf(peer types.PeerID) []MailboxHint {
	now := time.Now()
	s.mbxMu.Lock()
	hints, ok := s.mbx[peer]
	s.mbxMu.Unlock()
	if !ok {
		if raw, err := s.storage.Get(s.mailboxKey(peer)); err == nil {
			_ = json.Unmarshal(raw, &hints)
		}
	}
	return cleanHints(hints, now)
}

// RefreshMailboxHints asks the network (the DHT) for the peer's current record and returns the
// mailbox relays it names.
func (s *Swarm) RefreshMailboxHints(peer types.PeerID) []MailboxHint {
	s.mu.RLock()
	fetch := s.fetchBundle
	s.mu.RUnlock()
	if fetch != nil {
		if data, err := fetch(BundleKey(peer)); err == nil && len(data) > 0 {
			if _, err := s.bundleFromRecord(peer, data); err == nil {
				_ = s.storage.Set(s.bundleCacheKey(peer), data)
			}
		}
	}
	return s.MailboxHintsOf(peer)
}

// MailboxPeers is MailboxHintsOf as peer ids.
func (s *Swarm) MailboxPeers(peer types.PeerID) []types.PeerID {
	hints := s.MailboxHintsOf(peer)
	out := make([]types.PeerID, 0, len(hints))
	for _, h := range hints {
		b, _ := hex.DecodeString(h.Peer)
		var id types.PeerID
		copy(id[:], b)
		out = append(out, id)
	}
	return out
}
