package app

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/chat"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/convsync"
)

var (
	// ErrNoChat reports a chat reference that names nothing this node has.
	ErrNoChat = errors.New("app: unknown chat")
	// ErrChatOff reports a chat call on an app built without a conversation engine.
	ErrChatOff = errors.New("app: chat is not available")
)

// Delivery states of an own message, the same words the v1 chat screen knows.
const (
	StateOffline   = "offline"   // waits for a way to the recipient
	StateRelayed   = "relayed"   // a mailbox took it
	StateDelivered = "delivered" // the recipient's device confirmed it
	StateExpired   = "expired"   // given up
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
	Publish(ctx context.Context, c convlog.Conv, raw []byte) error
}

// StateSource tells the delivery state of an own event; ChatStates is the usual one.
type StateSource interface {
	StateOf(eventID string) (string, bool)
}

// SendNet is the sending side of chat/2 (*chat.Service): it returns the message id its status is
// reported under.
type SendNet interface {
	Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) ([16]byte, error)
}

// ChatStates keeps the delivery state of the events this node sent through chat/2.
type ChatStates struct{}

// Wrap gives the conversation engine a Net that sends through n and remembers which chat/2 message
// carries which event.
func (s *ChatStates) Wrap(n SendNet) convsync.Net { return passNet{n} }

type passNet struct{ n SendNet }

func (p passNet) Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) error {
	_, err := p.n.Send(ctx, to, m)
	return err
}

// NewChatStates returns an empty tracker.
func NewChatStates() *ChatStates { return &ChatStates{} }

// StateOf is the state of an event, if it was sent.
func (s *ChatStates) StateOf(string) (string, bool) { return "", false }

// OnStatus takes a status change of a chat/2 message.
func (s *ChatStates) OnStatus([16]byte, chat.State) {}

// Conversations lists the chats, newest first.
func (a *App) Conversations() ([]ConvEntry, error) { return nil, errors.New("not implemented") }

// ChatHistory returns a page of a chat, newest message first.
func (a *App) ChatHistory(ctx context.Context, ref ChatRef, cursor string, limit int) (ChatPage, error) {
	return ChatPage{}, errors.New("not implemented")
}

// ChatThread returns a page of the thread under root, oldest first.
func (a *App) ChatThread(ctx context.Context, ref ChatRef, root, cursor string, limit int) (ChatPage, error) {
	return ChatPage{}, errors.New("not implemented")
}

// ChatPins returns the pinned messages of a chat.
func (a *App) ChatPins(ctx context.Context, ref ChatRef) ([]convlog.Message, error) {
	return nil, errors.New("not implemented")
}

// ChatMessage returns one message.
func (a *App) ChatMessage(ctx context.Context, ref ChatRef, id string) (convlog.Message, error) {
	return convlog.Message{}, errors.New("not implemented")
}

// ChatSend sends a message.
func (a *App) ChatSend(ctx context.Context, ref ChatRef, req SendReq) (convlog.Message, error) {
	return convlog.Message{}, errors.New("not implemented")
}

// ChatEdit replaces the text of an own message.
func (a *App) ChatEdit(ctx context.Context, ref ChatRef, id, text string) error {
	return errors.New("not implemented")
}

// ChatDelete deletes a message for everyone.
func (a *App) ChatDelete(ctx context.Context, ref ChatRef, id string) error {
	return errors.New("not implemented")
}

// ChatReact sets or clears an own reaction.
func (a *App) ChatReact(ctx context.Context, ref ChatRef, id, emoji string, on bool) error {
	return errors.New("not implemented")
}

// ChatPin pins or unpins a message.
func (a *App) ChatPin(ctx context.Context, ref ChatRef, id string, on bool) error {
	return errors.New("not implemented")
}

// ChatMarkRead records that the chat was read up to id.
func (a *App) ChatMarkRead(ctx context.Context, ref ChatRef, id string) error {
	return errors.New("not implemented")
}

// SubscribeChats calls fn for every change of a chat until cancel is called.
func (a *App) SubscribeChats(fn func(ConvChange)) (cancel func()) { return func() {} }
