package node

import (
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// IncomingCall is a call invitation of a direct chat (T11.4b): the event the desktop shows as
// "call:incoming", or as "call:missed" when the invitation came too late to ring.
type IncomingCall struct {
	CallID      string `json:"callId"`
	Coordinator string `json:"coordinator"`
	From        string `json:"from"`      // the node ID of the caller
	Timestamp   int64  `json:"timestamp"` // when the invitation was written, ms
	Missed      bool   `json:"missed"`
}

// v2Now is the clock of the ring decision; a test replaces it.
var v2Now = time.Now

const (
	// ringWindow is how old an invitation may be and still ring (generous for clocks that differ).
	ringWindow = 2 * time.Minute
	// ringSeen bounds the invitations remembered so that one rings once.
	ringSeen = 512
)

// OnIncomingCall calls cb for every call invitation that arrives in a direct chat: a ringing one, or a
// missed one (it waited in a relay's mailbox). A later subscription replaces the earlier one, nil
// stops it. At protocol 1 it stays silent: there the invitation comes through MessageHandler.
//
// At protocol 2 the invitation is a chat message (InviteToCall) and arrives as a change of its chat;
// MessageHandler is closed. Who may ring is the v1 rule: a blocked contact never, a stranger only when
// the user lets everyone call, and nobody when the user lets nobody.
func (n *NodeApi) OnIncomingCall(cb func(IncomingCall)) {
	n.v2m.mu.Lock()
	n.v2m.ring = cb
	b := n.v2m.b
	n.v2m.mu.Unlock()
	if b != nil {
		b.watchCalls(cb, n.callsWho)
	}
}

// callsWho is the user's choice of who may call.
func (n *NodeApi) callsWho() string {
	if n.node == nil {
		return coreNode.DefaultPrivacy.Calls
	}
	return n.node.GetPrivacy().Calls
}

// watchCalls (re)subscribes the ring watcher to the chats.
func (b *v2MsgBackend) watchCalls(cb func(IncomingCall), who func() string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancelRing != nil {
		b.cancelRing()
		b.cancelRing = nil
	}
	if cb == nil {
		return
	}
	r := &ringer{b: b, cb: cb, who: who, seen: map[string]bool{}}
	b.cancelRing = b.app.SubscribeChats(r.change)
}

type ringer struct {
	b   *v2MsgBackend
	cb  func(IncomingCall)
	who func() string

	mu    sync.Mutex
	seen  map[string]bool
	order []string
}

// first reports whether id has not been seen, and remembers it.
func (r *ringer) first(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == "" || r.seen[id] {
		return false
	}
	r.seen[id] = true
	r.order = append(r.order, id)
	if len(r.order) > ringSeen {
		delete(r.seen, r.order[0])
		r.order = r.order[1:]
	}
	return true
}

func (r *ringer) allowed(c app.ConvChange) bool {
	if c.Conversation.Blocked {
		return false
	}
	switch r.who() {
	case coreNode.WhoNobody:
		return false
	case coreNode.WhoEveryone:
		return true
	default:
		return !c.Conversation.Request
	}
}

func (r *ringer) change(c app.ConvChange) {
	if c.Ref.Kind != app.KindDM {
		return
	}
	for _, m := range c.Messages {
		if m.Mine || m.Deleted {
			continue
		}
		id, coord, ok := ParseCallInvite(m.Text)
		if !ok || !r.first(m.ID) || !r.allowed(c) {
			continue
		}
		from := r.b.names().of(m.Author)
		r.cb(IncomingCall{CallID: id, Coordinator: coord, From: from, Timestamp: m.TS,
			Missed: v2Now().Sub(time.UnixMilli(m.TS)) > ringWindow})
	}
}
