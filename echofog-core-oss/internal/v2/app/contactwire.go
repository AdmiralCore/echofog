package app

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	apitypes "github.com/echofog/echofog-core/pkg/api/types"
)

const (
	// knockInterval is the least time between two answers to one contact's knocks.
	knockInterval = time.Minute
	// maxKnockCard bounds a card answer handed on: a card envelope is a few KB.
	maxKnockCard = 32 << 10
)

// Connected implements the v1 contacts.Wire port for v2: whether there is a live session with
// the node (T7.0, AD15).
func (a *App) Connected(id apitypes.PeerID) bool {
	for _, s := range a.cfg.Runtime.Status().Sessions {
		if [32]byte(s.Node) == [32]byte(id) {
			return true
		}
	}
	return false
}

// known reports whether node is a contact that is not blocked. A stranger and a blocked contact
// are treated alike by every control handler: the sender learns nothing about which it is.
func (a *App) known(node [32]byte) bool {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	r, err := a.loadContact(hex.EncodeToString(node[:]))
	return err == nil && !r.Blocked
}

// Knock asks a contact for its current card (chat/2 Control.knock).
func (a *App) Knock(ctx context.Context, node string) error {
	var id [32]byte
	if b, err := hex.DecodeString(node); err != nil || len(b) != 32 {
		return ErrNoContact
	} else {
		copy(id[:], b)
	}
	a.cmu.Lock()
	r, err := a.loadContact(node)
	a.cmu.Unlock()
	if err != nil {
		return err
	}
	if r.Blocked {
		return ErrBlocked
	}
	if a.cfg.Chat == nil {
		return errors.New("app: no chat service")
	}
	_, err = a.cfg.Chat.Send(ctx, id, &chatpb.PersonalMessageProto{Control: &chatpb.Control{
		Kind: &chatpb.Control_Knock{Knock: &chatpb.Knock{Ask: true}}}})
	return err
}

// OnControl handles a chat/2 control message from an authenticated account. Wire it to
// chat.Events.Control. Only contacts are served: a knock from anyone else, or from a blocked
// contact, gets no answer at all. An answer to one's own knock is passed on bounded; the
// receiver verifies the card.
func (a *App) OnControl(from [32]byte, c *chatpb.Control) {
	k := c.GetKnock()
	if k == nil || a.cfg.Chat == nil || !a.known(from) {
		return
	}
	if !k.GetAsk() {
		if card := k.GetCard(); len(card) > 0 && len(card) <= maxKnockCard && a.cfg.IngestCard != nil {
			a.cfg.IngestCard(from, card)
		}
		return
	}
	if a.cfg.OwnCard == nil || !a.allowKnock(from) {
		return
	}
	card, ok := a.cfg.OwnCard()
	if !ok {
		return
	}
	_, _ = a.cfg.Chat.Send(context.Background(), from, &chatpb.PersonalMessageProto{Control: &chatpb.Control{
		Kind: &chatpb.Control_Knock{Knock: &chatpb.Knock{Card: card}}}})
}

// allowKnock rate-limits the answers to one contact. The table only holds contacts, so the
// contact limit bounds it.
func (a *App) allowKnock(from [32]byte) bool {
	now := a.cfg.Clock.Now()
	a.kmu.Lock()
	defer a.kmu.Unlock()
	if last, ok := a.knocked[from]; ok && now.Sub(last) < knockInterval && !now.Before(last) {
		return false
	}
	if a.knocked == nil {
		a.knocked = map[[32]byte]time.Time{}
	}
	if len(a.knocked) >= a.maxContacts() {
		for k, t := range a.knocked { // drop the stale ones; the table must not outgrow the list
			if now.Sub(t) >= knockInterval {
				delete(a.knocked, k)
			}
		}
		if len(a.knocked) >= a.maxContacts() {
			return false
		}
	}
	a.knocked[from] = now
	return true
}
