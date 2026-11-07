package app

import (
	"context"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/chat"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/pkg/api/types"
)

var (
	// ErrNoChat reports a chat reference that names nothing this node has.
	ErrNoChat = errors.New("app: unknown chat")
	// ErrChatOff reports a chat call on an app built without a conversation engine.
	ErrChatOff = errors.New("app: chat is not available")
	// ErrBadMessage reports a message id that is not one.
	ErrBadMessage = errors.New("app: bad message id")
	// ErrEmptyMessage reports a message with no text.
	ErrEmptyMessage = errors.New("app: empty message")
	// ErrMessageTooLong reports a message over MaxMessageText bytes.
	ErrMessageTooLong = errors.New("app: message is too long")
)

// MaxMessageText bounds the text of one message (the log takes events up to 64 KiB).
const MaxMessageText = 16 << 10

// Delivery states of an own message, the same words the v1 chat screen knows.
const (
	StateOffline   = "offline"   // waits for a way to the recipient
	StateRelayed   = "relayed"   // a mailbox took it
	StateDelivered = "delivered" // the recipient's device confirmed it
	StateExpired   = "expired"   // given up
)

// Chat kinds of a ChatRef.
const (
	KindDM    = convlog.KindDM
	KindGroup = convlog.KindGroup
)

// ChatRef names a chat: Kind "dm" (ID is the contact's hex node ID) or "group" (ID is the group
// ID); Channel is a group's channel, "" for the main one.
type ChatRef struct {
	Kind    string
	ID      string
	Channel string
}

// ConvEntry is one line of the chat list.
type ConvEntry struct {
	Ref      ChatRef
	Conv     string
	Last     *convlog.Message
	LastTS   int64
	Unread   int
	Request  bool // a stranger's chat
	Upgraded bool
	Kind     string // "direct" or "group"
	Name     string
	PeerID   string
	Online   bool
	Blocked  bool
}

// ChatPage is one page of a chat or a thread.
type ChatPage struct {
	Messages []convlog.Message
	Next     string
}

// SendReq is a message to send.
type SendReq struct {
	Text    string
	ReplyTo string
	Thread  string
}

// ConvChange is pushed when a chat changes.
type ConvChange struct {
	Ref          ChatRef
	Conversation ConvEntry
	Messages     []convlog.Message
	Reload       bool
	Rev          uint64
}

// Convs is the part of *convsync.Engine the chat uses.
type Convs interface {
	Log() *convlog.Log
	DM(peer [32]byte) convlog.Conv
	PubOfPeer(peer types.PeerID) ([32]byte, bool)
	Publish(ctx context.Context, c convlog.Conv, raw []byte) error
}

// StateSource tells the delivery state of an own event; *ChatStates is the usual one.
type StateSource interface {
	StateOf(eventID string) (string, bool)
}

// SendNet is the sending side of chat/2 (*chat.Service): it returns the message id its status is
// reported under.
type SendNet interface {
	Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) ([16]byte, error)
}

const maxTracked = 8192

// ChatStates keeps the delivery state of the events this node sent through chat/2. It is bounded:
// the oldest records go first.
type ChatStates struct {
	mu    sync.Mutex
	byMsg map[[16]byte]string // chat/2 message id -> event id
	state map[string]string   // event id -> state
	order []string
}

// NewChatStates returns an empty tracker.
func NewChatStates() *ChatStates {
	return &ChatStates{byMsg: map[[16]byte]string{}, state: map[string]string{}}
}

// Wrap gives the conversation engine a Net that sends through n and remembers which chat/2 message
// carries which event.
func (s *ChatStates) Wrap(n SendNet) convsync.Net { return trackNet{s: s, n: n} }

type trackNet struct {
	s *ChatStates
	n SendNet
}

func (t trackNet) Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) error {
	id, err := t.n.Send(ctx, to, m)
	if err != nil {
		return err
	}
	if raw := m.GetConvEvent(); len(raw) > 0 {
		if _, eid, derr := convlog.Decode(raw); derr == nil {
			t.s.note(id, eid.String())
		}
	}
	return nil
}

func (s *ChatStates) note(msg [16]byte, event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.state[event]; !seen {
		s.order = append(s.order, event)
		for len(s.order) > maxTracked {
			old := s.order[0]
			s.order = s.order[1:]
			delete(s.state, old)
			for k, v := range s.byMsg {
				if v == old {
					delete(s.byMsg, k)
				}
			}
		}
		s.state[event] = StateOffline
	}
	s.byMsg[msg] = event
}

// StateOf is the state of an event, if this node sent it.
func (s *ChatStates) StateOf(eventID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.state[eventID]
	return st, ok
}

// OnStatus takes a status change of a chat/2 message. A delivered message never goes back.
func (s *ChatStates) OnStatus(id [16]byte, st chat.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev, ok := s.byMsg[id]
	if !ok {
		return
	}
	var v string
	switch st {
	case chat.StateWaiting:
		v = StateOffline
	case chat.StateMailbox:
		v = StateRelayed
	case chat.StateDelivered:
		v = StateDelivered
	case chat.StateFailed:
		v = StateExpired
	default:
		return
	}
	if s.state[ev] == StateDelivered {
		return
	}
	s.state[ev] = v
}

// chatState holds the subscribers of chat changes.
type chatState struct {
	once sync.Once
	mu   sync.Mutex
	subs map[int]func(ConvChange)
	next int
}

func (a *App) convs() (Convs, error) {
	if a.cfg.Convs == nil {
		return nil, ErrChatOff
	}
	return a.cfg.Convs, nil
}

func nodeBytes(s string) ([32]byte, bool) {
	var n [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return n, false
	}
	copy(n[:], b)
	return n, true
}

// resolve turns a reference into a conversation. write is true for calls that send: they need a
// contact who is not blocked.
func (a *App) resolve(ctx context.Context, ref ChatRef, write bool) (convlog.Conv, error) {
	cv, err := a.convs()
	if err != nil {
		return convlog.Conv{}, err
	}
	if ref.Kind == KindGroup {
		return a.resolveGroup(cv, ref)
	}
	if ref.Kind != KindDM {
		return convlog.Conv{}, ErrNoChat
	}
	node, ok := nodeBytes(ref.ID)
	if !ok {
		return convlog.Conv{}, ErrNoChat
	}
	if write {
		a.cmu.Lock()
		r, lerr := a.loadContact(ref.ID)
		a.cmu.Unlock()
		switch {
		case lerr != nil:
			return convlog.Conv{}, ErrNoContact
		case r.Blocked:
			return convlog.Conv{}, ErrBlocked
		}
	}
	if a.cfg.PubOf == nil {
		return convlog.Conv{}, ErrNoChat
	}
	pub, err := a.cfg.PubOf(ctx, node)
	if err != nil || convsync.NodeOf(pub) != node {
		return convlog.Conv{}, ErrNoChat
	}
	c := cv.DM(pub)
	a.savePub(c.Peer, pub)
	if err := cv.Log().Open(c); err != nil {
		return convlog.Conv{}, err
	}
	return c, nil
}

func parseEvent(s string) (convlog.EventID, error) {
	if s == "" {
		return convlog.EventID{}, nil
	}
	id, err := convlog.ParseEventID(s)
	if err != nil {
		return convlog.EventID{}, ErrBadMessage
	}
	return id, nil
}

func requireEvent(s string) (convlog.EventID, error) {
	id, err := parseEvent(s)
	if err == nil && id.IsZero() {
		err = ErrBadMessage
	}
	return id, err
}

// dress fills in the delivery state of own messages.
func (a *App) dress(msgs []convlog.Message) []convlog.Message {
	for i := range msgs {
		if !msgs[i].Mine || msgs[i].Legacy {
			continue
		}
		if a.cfg.States != nil {
			if s, ok := a.cfg.States.StateOf(msgs[i].ID); ok {
				msgs[i].State = s
				continue
			}
		}
		// sent in an earlier run: nothing here says it arrived, and "delivered" is never guessed
		msgs[i].State = StateOffline
	}
	return msgs
}

const peerPrefix = "cp/"

func (a *App) savedPub(p types.PeerID) ([32]byte, bool) {
	v, err := a.cfg.Store.Get([]byte(peerPrefix + hex.EncodeToString(p[:])))
	if err != nil || len(v) != 32 {
		return [32]byte{}, false
	}
	return [32]byte(v), true
}

func (a *App) savePub(p types.PeerID, pub [32]byte) {
	_ = a.cfg.Store.Put([]byte(peerPrefix+hex.EncodeToString(p[:])), pub[:])
}

// refOf names the chat of a conversation; the node of a direct chat is found through the engine
// or the keys saved from earlier runs.
func (a *App) refOf(c convlog.Conv) (ChatRef, bool) {
	if c.Kind == KindGroup {
		return ChatRef{Kind: KindGroup, ID: c.Group}, true
	}
	cv, err := a.convs()
	if err != nil {
		return ChatRef{}, false
	}
	pub, ok := cv.PubOfPeer(c.Peer)
	if ok {
		a.savePub(c.Peer, pub)
	} else if pub, ok = a.savedPub(c.Peer); !ok {
		return ChatRef{}, false
	}
	n := convsync.NodeOf(pub)
	return ChatRef{Kind: KindDM, ID: hex.EncodeToString(n[:])}, true
}

func (a *App) entry(c convlog.Conv, meta convlog.Meta) (ConvEntry, bool) {
	ref, ok := a.refOf(c)
	if !ok {
		return ConvEntry{}, false
	}
	e := ConvEntry{Ref: ref, Conv: meta.Conv, LastTS: meta.LastTS, Unread: meta.UnreadAll(), Upgraded: true}
	cv, _ := a.convs()
	if meta.LastID != "" {
		if id, err := convlog.ParseEventID(meta.LastID); err == nil {
			if m, ok := cv.Log().Message(c, id); ok {
				m = a.dress([]convlog.Message{m})[0]
				e.Last = &m
			}
		}
	}
	if c.Kind == KindDM {
		e.Kind = "direct"
		e.PeerID = ref.ID
		e.Name = ref.ID
		a.cmu.Lock()
		r, err := a.loadContact(ref.ID)
		a.cmu.Unlock()
		e.Request = err != nil
		if err == nil {
			e.Blocked = r.Blocked
			e.Name = r.Alias
			if node, ok := nodeBytes(ref.ID); ok {
				e.Online = a.Connected(types.PeerID(node))
			}
		}
	} else {
		e.Kind = "group"
		if a.cfg.Groups == nil {
			return ConvEntry{}, false
		}
		r, ok := a.cfg.Groups.rec(c.Group)
		if !ok {
			return ConvEntry{}, false
		}
		e.Name = r.Genesis.Title
	}
	return e, true
}

// Conversations lists the chats, the newest first.
func (a *App) Conversations() ([]ConvEntry, error) {
	cv, err := a.convs()
	if err != nil {
		return nil, err
	}
	metas, err := cv.Log().Conversations()
	if err != nil {
		return nil, err
	}
	var out []ConvEntry
	for _, m := range metas {
		c, ok := convOfMeta(m)
		if !ok {
			continue
		}
		if e, ok := a.entry(c, m); ok {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	return out, nil
}

func convOfMeta(m convlog.Meta) (convlog.Conv, bool) {
	id, err := convlog.ParseConvID(m.Conv)
	if err != nil {
		return convlog.Conv{}, false
	}
	switch m.Kind {
	case convlog.KindDM:
		raw, err := hex.DecodeString(m.Peer)
		if err != nil || len(raw) != len(types.PeerID{}) {
			return convlog.Conv{}, false
		}
		var peer types.PeerID
		copy(peer[:], raw)
		return convlog.Conv{ID: id, Kind: convlog.KindDM, Peer: peer}, true
	case convlog.KindGroup:
		return convlog.Conv{ID: id, Kind: convlog.KindGroup, Group: m.Group}, true
	}
	return convlog.Conv{}, false
}

// ChatHistory returns a page of a chat, newest message first.
func (a *App) ChatHistory(ctx context.Context, ref ChatRef, cursor string, limit int) (ChatPage, error) {
	c, err := a.resolve(ctx, ref, false)
	if err != nil {
		return ChatPage{}, err
	}
	cv, _ := a.convs()
	msgs, next, err := cv.Log().HistoryIn(c, ref.Channel, cursor, limit)
	if err != nil {
		return ChatPage{}, err
	}
	return ChatPage{Messages: a.dress(msgs), Next: next}, nil
}

// ChatThread returns a page of the thread under root, oldest first.
func (a *App) ChatThread(ctx context.Context, ref ChatRef, root, cursor string, limit int) (ChatPage, error) {
	c, err := a.resolve(ctx, ref, false)
	if err != nil {
		return ChatPage{}, err
	}
	rid, err := requireEvent(root)
	if err != nil {
		return ChatPage{}, err
	}
	cv, _ := a.convs()
	msgs, next, err := cv.Log().Thread(c, rid, cursor, limit)
	if err != nil {
		return ChatPage{}, err
	}
	return ChatPage{Messages: a.dress(msgs), Next: next}, nil
}

// ChatPins returns the pinned messages of a chat, the latest pinned first.
func (a *App) ChatPins(ctx context.Context, ref ChatRef) ([]convlog.Message, error) {
	c, err := a.resolve(ctx, ref, false)
	if err != nil {
		return nil, err
	}
	cv, _ := a.convs()
	msgs, err := cv.Log().Pins(c)
	return a.dress(msgs), err
}

// ChatMessage returns one message.
func (a *App) ChatMessage(ctx context.Context, ref ChatRef, id string) (convlog.Message, error) {
	c, err := a.resolve(ctx, ref, false)
	if err != nil {
		return convlog.Message{}, err
	}
	eid, err := requireEvent(id)
	if err != nil {
		return convlog.Message{}, err
	}
	cv, _ := a.convs()
	m, ok := cv.Log().Message(c, eid)
	if !ok {
		return convlog.Message{}, convlog.ErrNotFound
	}
	return a.dress([]convlog.Message{m})[0], nil
}

// ChatSend sends a message: plain, an answer to another (ReplyTo) or a comment in a thread.
func (a *App) ChatSend(ctx context.Context, ref ChatRef, req SendReq) (convlog.Message, error) {
	c, err := a.resolve(ctx, ref, true)
	if err != nil {
		return convlog.Message{}, err
	}
	switch {
	case strings.TrimSpace(req.Text) == "":
		return convlog.Message{}, ErrEmptyMessage
	case len(req.Text) > MaxMessageText:
		return convlog.Message{}, ErrMessageTooLong
	}
	reply, err := parseEvent(req.ReplyTo)
	if err != nil {
		return convlog.Message{}, err
	}
	thread, err := parseEvent(req.Thread)
	if err != nil {
		return convlog.Message{}, err
	}
	cv, _ := a.convs()
	raw, id, err := cv.Log().AppendMessageIn(c, ref.Channel, req.Text, 0, "", reply, thread)
	if err != nil {
		return convlog.Message{}, err
	}
	sendErr := cv.Publish(ctx, c, raw)
	m, _ := cv.Log().Message(c, id)
	return a.dress([]convlog.Message{m})[0], sendErr
}

func (a *App) chatOp(ctx context.Context, ref ChatRef, target string, do func(*convlog.Log, convlog.Conv, convlog.EventID) ([]byte, error)) error {
	c, err := a.resolve(ctx, ref, true)
	if err != nil {
		return err
	}
	tid, err := requireEvent(target)
	if err != nil {
		return err
	}
	cv, _ := a.convs()
	raw, err := do(cv.Log(), c, tid)
	if err != nil {
		return err
	}
	return cv.Publish(ctx, c, raw)
}

// ChatEdit replaces the text of an own message.
func (a *App) ChatEdit(ctx context.Context, ref ChatRef, id, text string) error {
	if strings.TrimSpace(text) == "" {
		return ErrEmptyMessage
	}
	if len(text) > MaxMessageText {
		return ErrMessageTooLong
	}
	return a.chatOp(ctx, ref, id, func(l *convlog.Log, c convlog.Conv, t convlog.EventID) ([]byte, error) {
		raw, _, err := l.AppendEdit(c, t, text)
		return raw, err
	})
}

// ChatDelete deletes a message for everyone.
func (a *App) ChatDelete(ctx context.Context, ref ChatRef, id string) error {
	return a.chatOp(ctx, ref, id, func(l *convlog.Log, c convlog.Conv, t convlog.EventID) ([]byte, error) {
		raw, _, err := l.AppendDelete(c, t)
		return raw, err
	})
}

// ChatReact sets or clears an own reaction.
func (a *App) ChatReact(ctx context.Context, ref ChatRef, id, emoji string, on bool) error {
	return a.chatOp(ctx, ref, id, func(l *convlog.Log, c convlog.Conv, t convlog.EventID) ([]byte, error) {
		raw, _, err := l.AppendReact(c, t, emoji, on)
		return raw, err
	})
}

// ChatPin pins or unpins a message.
func (a *App) ChatPin(ctx context.Context, ref ChatRef, id string, on bool) error {
	return a.chatOp(ctx, ref, id, func(l *convlog.Log, c convlog.Conv, t convlog.EventID) ([]byte, error) {
		raw, _, err := l.AppendPin(c, t, on)
		return raw, err
	})
}

// ChatMarkRead records that the chat was read up to id. It writes only the reader's own position
// and sends nothing: a read receipt travels only over a live session, as a chat/2 control.
func (a *App) ChatMarkRead(ctx context.Context, ref ChatRef, id string) error {
	c, err := a.resolve(ctx, ref, false)
	if err != nil {
		return err
	}
	eid, err := requireEvent(id)
	if err != nil {
		return err
	}
	cv, _ := a.convs()
	return cv.Log().MarkReadIn(c, ref.Channel, eid)
}

// SubscribeChats calls fn for every change of a chat until cancel is called.
func (a *App) SubscribeChats(fn func(ConvChange)) (cancel func()) {
	a.chats.once.Do(a.hookChats)
	a.chats.mu.Lock()
	defer a.chats.mu.Unlock()
	if a.chats.subs == nil {
		a.chats.subs = map[int]func(ConvChange){}
	}
	a.chats.next++
	id := a.chats.next
	a.chats.subs[id] = fn
	return func() {
		a.chats.mu.Lock()
		delete(a.chats.subs, id)
		a.chats.mu.Unlock()
	}
}

func (a *App) hookChats() {
	cv, err := a.convs()
	if err != nil {
		return
	}
	cv.Log().OnChange(a.onChatChange)
}

func (a *App) onChatChange(ch convlog.Change) {
	e, ok := a.entry(ch.Conv, ch.Meta)
	if !ok {
		return
	}
	a.chats.mu.Lock()
	subs := make([]func(ConvChange), 0, len(a.chats.subs))
	for _, f := range a.chats.subs {
		subs = append(subs, f)
	}
	a.chats.mu.Unlock()
	out := ConvChange{Ref: e.Ref, Conversation: e, Messages: a.dress(ch.Messages), Reload: ch.Rebuilt || len(ch.Members) > 0, Rev: ch.Rev}
	for _, f := range subs {
		f(out)
	}
}

// resolveGroup turns a group reference into its conversation. A group this node does not keep, one
// it was never let into and a name that is none look the same: ErrNoChat.
func (a *App) resolveGroup(cv Convs, ref ChatRef) (convlog.Conv, error) {
	if a.cfg.Groups == nil {
		return convlog.Conv{}, ErrNoChat
	}
	if _, ok := a.cfg.Groups.rec(ref.ID); !ok {
		return convlog.Conv{}, ErrNoChat
	}
	c := convlog.Group(ref.ID)
	if err := cv.Log().Open(c); err != nil {
		return convlog.Conv{}, ErrNoChat
	}
	if !cv.Log().Participant(c, cv.Log().Self()) {
		return convlog.Conv{}, ErrNoChat
	}
	return c, nil
}
