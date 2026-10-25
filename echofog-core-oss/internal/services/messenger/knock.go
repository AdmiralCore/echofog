package messenger

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// A knock carries this node's signed identity card to a peer whose keys it does not have, over
// whatever carries chat messages: a direct link, a relay, or a relay's mailbox while the peer is
// away. The first message to somebody needs their pre-key bundle. It is published in the DHT, but
// a DHT that answers slowly or not at all (a small network, a routing table full of nodes that are
// gone) left that message waiting for keys for ever, while the relays would have carried it. The
// receiver checks the card (it must belong to the signer of the envelope and be signed by it),
// keeps it as the sender's bundle, answers with its own card, and both sides can encrypt what
// waits in their outboxes.
const (
	knockPrefix = "fogknock1:"
	knockAsk    = "q" // please answer with your card
	knockAnswer = "a" // here is mine, no answer needed
	knockPause  = 30 * time.Second
)

func knockText(flag string, card []byte) string {
	return knockPrefix + flag + ":" + base64.StdEncoding.EncodeToString(card)
}

// parseKnock reads a knock; ok is false for anything else.
func parseKnock(text string) (flag string, card []byte, ok bool) {
	rest, found := strings.CutPrefix(text, knockPrefix)
	if !found {
		return "", nil, false
	}
	flag, enc, found := strings.Cut(rest, ":")
	if !found || (flag != knockAsk && flag != knockAnswer) {
		return "", nil, false
	}
	card, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(card) == 0 {
		return "", nil, false
	}
	return flag, card, true
}

// knockDue says whether an ask may go to target now: one per knockPause per peer.
func (s *MessageBroker) knockDue(target types.PeerID) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.lastKnock == nil || len(s.lastKnock) > 4096 {
		s.lastKnock = make(map[types.PeerID]time.Time)
	}
	if last, ok := s.lastKnock[target]; ok && time.Since(last) < knockPause {
		return false
	}
	s.lastKnock[target] = time.Now()
	return true
}

// sendKnock sends our card to target. An ask goes at most once per knockPause per peer; an answer
// goes at once. The knock rides the plain tier: there is no session to encrypt it with yet, and
// the card carries its own signature.
func (s *MessageBroker) sendKnock(target types.PeerID, answer bool) {
	if !answer && !s.knockDue(target) {
		return
	}
	card, err := s.sw.IdentityCard()
	if err != nil {
		return
	}
	flag := knockAsk
	if answer {
		flag = knockAnswer
	}
	_, state, err := s.sendWithTier(target, knockText(flag, card), TierPlain, SendOptions{Silent: true, noTrack: true})
	switch {
	case err != nil:
		s.log.Info("MessageBroker: could not send our keys to the peer", "peer", target.String()[:8], "err", err)
	case state != StateSent:
		s.log.Info("MessageBroker: nothing to carry our keys to the peer yet", "peer", target.String()[:8])
	default:
		s.log.Info("MessageBroker: sent our keys to the peer", "peer", target.String()[:8], "answer", answer)
	}
}

// handleKnock keeps the card a peer sent as its bundle, lets what waited for its keys go out, and
// answers with our card when asked. A peer that may not write to us (blocked, or a stranger where
// strangers may not write) gets no answer.
func (s *MessageBroker) handleKnock(origin types.PeerID, text string) {
	flag, card, ok := parseKnock(text)
	if !ok {
		return
	}
	if err := s.sw.CacheBundle(origin, card); err != nil {
		s.log.Debug("MessageBroker: a knock carried a bad card", "peer", origin.String()[:8], "err", err)
		return
	}
	s.log.Info("MessageBroker: got the keys of a peer", "peer", origin.String()[:8])
	s.mu.RLock()
	filter := s.filter
	s.mu.RUnlock()
	if filter != nil && filter(origin) == Drop {
		return
	}
	go s.FlushOutbox(origin)
	if flag == knockAsk {
		s.sendKnock(origin, true)
	}
}
