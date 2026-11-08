package scenarios

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/chat"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/links"
)

// Inbox records what chat/2 told the node: every message that arrived (and how many times) and the
// last state of every message it sent.
type Inbox struct {
	mu       sync.Mutex
	received map[[16]byte]int
	text     map[[16]byte]string
	states   map[[16]byte]chat.State
	order    [][16]byte
}

func newInbox() *Inbox {
	return &Inbox{received: map[[16]byte]int{}, text: map[[16]byte]string{}, states: map[[16]byte]chat.State{}}
}

func (b *Inbox) events() chat.Events {
	return chat.Events{
		Message: func(in chat.Incoming) {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.received[in.ID] == 0 {
				b.order = append(b.order, in.ID)
			}
			b.received[in.ID]++
			b.text[in.ID] = in.Message.GetText()
		},
		Status: func(id [16]byte, st chat.State) {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.states[id] = st
		},
	}
}

// Received is how many times the message arrived.
func (b *Inbox) Received(id [16]byte) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.received[id]
}

// Count is the number of distinct messages that arrived, and the most times any of them did.
func (b *Inbox) Count() (distinct, maxTimes int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range b.received {
		maxTimes = max(maxTimes, n)
	}
	return len(b.received), maxTimes
}

// State is the last state the sender was told for a message it sent.
func (b *Inbox) State(id [16]byte) chat.State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.states[id]
}

// Text is the text of a received message.
func (b *Inbox) Text(id [16]byte) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text[id]
}

// Contact makes b a contact of a through a's contact link: a real handshake over the real carrier,
// through links.Service and app.App, as the application does it.
func (a *Node) Contact(ctx context.Context, b *Node) error {
	if a.RT.Chat() == nil || b.RT.Chat() == nil {
		return errors.New("scenarios: both nodes need chat")
	}
	if err := a.ensureApp(); err != nil {
		return err
	}
	if err := b.ensureApp(); err != nil {
		return err
	}
	link, err := b.links.ContactLink(ctx)
	if err != nil {
		return fmt.Errorf("contact link: %w", err)
	}
	if _, err := a.app.AddByLink(ctx, link.URL); err != nil {
		return fmt.Errorf("add by link: %w", err)
	}
	return nil
}

func (n *Node) ensureApp() error {
	if n.app != nil {
		return nil
	}
	ln, err := links.New(links.Config{Cards: n.builder, Dialer: n.RT, Clock: n.RT.Clock()})
	if err != nil {
		return err
	}
	ap, err := app.New(app.Config{
		Runtime: n.RT, Links: ln, Store: n.RT.Store("t15-app"), Clock: n.RT.Clock(),
		Self: [32]byte(n.ID.NodeID), Chat: n.RT.Chat(),
	})
	if err != nil {
		return err
	}
	n.links, n.app = ln, ap
	return nil
}

// SendText puts a text message for to in n's outbox and returns its id.
func (n *Node) SendText(ctx context.Context, to *Node, text string) ([16]byte, error) {
	return n.RT.Chat().Send(ctx, [32]byte(to.ID.NodeID), &chatpb.PersonalMessageProto{Text: text})
}
