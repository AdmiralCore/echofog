package scenarios

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/chat"
	chatcrypto "github.com/echofog/echofog-core/internal/v2/chat/crypto"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/store"
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

// labDirectory is the Directory of the lab's chat services: it answers from the nodes of the world,
// in place of the DHT. A loopback world cannot give the DHT the quorum and the network diversity it
// wants (the lookup stays inconclusive), and the directory is not what the scenario measures; the
// chat service, its crypto, the outbox, the stream to the peer and the receipt are the product's.
type labDirectory struct{ w *World }

func (d labDirectory) Resolve(_ context.Context, node [32]byte) (*dht.Resolved, error) {
	n := d.w.byAccount(node)
	if n == nil || n.chatKeys == nil {
		return nil, errors.New("scenarios: no such node with chat")
	}
	now := time.Now()
	dl, err := objects.VerifyDeviceList(n.devRaw, now)
	if err != nil {
		return nil, err
	}
	raw, err := n.chatKeys.Prekeys()
	if err != nil {
		return nil, err
	}
	pk, err := objects.VerifyPrekeys(raw, now)
	if err != nil {
		return nil, err
	}
	return &dht.Resolved{Devices: dl, Prekeys: []*objects.Prekeys{pk}}, nil
}

func (w *World) byAccount(node [32]byte) *Node {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range w.nodes {
		if [32]byte(n.ID.NodeID) == node {
			return n
		}
	}
	return nil
}

// attachChat gives the node chat/2 as node.registerChat does, with the lab's directory. The service is
// registered with the runtime, so the handshake offers it and a peer's DELIVER reaches this service.
func (w *World) attachChat(n *Node) error {
	spec, ok := service.Builtin(service.Chat)
	if !ok {
		return errors.New("scenarios: chat/2 is not built in")
	}
	ids, err := store.NewIDs(n.tw.Account.StorageKey())
	if err != nil {
		return err
	}
	cr, err := chatcrypto.New(chatcrypto.Config{Device: n.tw.Device, Store: n.store.Store, IDs: ids, Now: time.Now})
	if err != nil {
		ids.Close()
		return err
	}
	rt := n.RT
	svc, err := chat.NewService(chat.ServiceConfig{
		Self:   chat.Self{Node: [32]byte(n.ID.NodeID), Device: [32]byte(n.ID.DeviceID), Account: n.tw.Account.Public(), DeviceKey: n.tw.Device.Public()},
		Crypto: cr, Dir: labDirectory{w},
		Link: &chat.StreamLink{Open: func(ctx context.Context, device [32]byte) (chat.Stream, error) {
			_, st, err := rt.Open(ctx, node.DeviceID(device), service.Chat, node.DialOpt{})
			if err != nil {
				return nil, err
			}
			return st, nil
		}},
		Store: n.store.Store("v2/chat/outbox"), Now: time.Now, Events: n.Inbox.events(),
	})
	if err != nil {
		cr.Close()
		ids.Close()
		return err
	}
	srv := chat.New(chat.Config{Bundles: cr, Receiver: svc, Now: time.Now})
	rt.Register(spec, func(ctx context.Context, p node.Peer, st *mux.Stream) {
		srv.Handle(ctx, chat.Peer{SID: p.SID, Path: p.Path, Remote: p.Remote}, st)
	})
	n.chat, n.chatKeys = svc, cr
	n.closers = append(n.closers, func() { cr.Close(); ids.Close() })
	return nil
}

// tickChat runs the outbox as the runtime does, until the node is closed.
func (n *Node) tickChat(ctx context.Context) {
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = n.chat.Tick(ctx)
			}
		}
	}()
}

// SendText puts a text message for to in n's outbox and returns its id.
func (n *Node) SendText(ctx context.Context, to *Node, text string) ([16]byte, error) {
	return n.chat.Send(ctx, [32]byte(to.ID.NodeID), &chatpb.PersonalMessageProto{Text: text})
}
