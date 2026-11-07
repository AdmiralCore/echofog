package app_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/chat"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/store"
	"google.golang.org/protobuf/proto"
)

// ---- two nodes over one in-memory chat/2 -----------------------------------------------------------------------

type chatNode struct {
	pub  [32]byte
	priv ed25519.PrivateKey
	node [32]byte
	eng  *convsync.Engine
	app  *app.App
	rig  *rig
	st   *app.ChatStates
	port *chatPort
}

type letter struct {
	from, to [32]byte
	pm       *chatpb.PersonalMessageProto
}

type chatNet struct {
	mu    sync.Mutex
	queue []letter
	nodes map[[32]byte]*chatNode
	pubs  map[[32]byte][32]byte
	down  map[[32]byte]bool // nodes that cannot be reached
}

type chatPort struct {
	n    *chatNet
	self [32]byte
	seq  uint64
	last [16]byte
}

func (p *chatPort) Send(_ context.Context, to [32]byte, m *chatpb.PersonalMessageProto) ([16]byte, error) {
	p.n.mu.Lock()
	defer p.n.mu.Unlock()
	if p.n.down[to] {
		return [16]byte{}, errors.New("unreachable")
	}
	p.seq++
	var id [16]byte
	binary.BigEndian.PutUint64(id[8:], p.seq)
	p.last = id
	p.n.queue = append(p.n.queue, letter{p.self, to, proto.Clone(m).(*chatpb.PersonalMessageProto)})
	return id, nil
}

func (n *chatNet) Pub(_ context.Context, node [32]byte) ([32]byte, error) {
	if pub, ok := n.pubs[node]; ok {
		return pub, nil
	}
	return [32]byte{}, errors.New("unknown")
}

func (n *chatNet) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 10000; i++ {
		n.mu.Lock()
		if len(n.queue) == 0 {
			n.mu.Unlock()
			return
		}
		l := n.queue[0]
		n.queue = n.queue[1:]
		dst := n.nodes[l.to]
		n.mu.Unlock()
		if dst != nil {
			dst.eng.Receive(context.Background(), l.from, l.pm)
		}
	}
	t.Fatal("the network never went quiet")
}

func newChatPair(t *testing.T) (*chatNet, *chatNode, *chatNode) {
	t.Helper()
	net := &chatNet{nodes: map[[32]byte]*chatNode{}, pubs: map[[32]byte][32]byte{}, down: map[[32]byte]bool{}}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sk := secret.New(key)
	ids, err := store.NewIDs(sk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ids.Close(); sk.Destroy() })
	var out []*chatNode
	for i := 0; i < 2; i++ {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		c := &chatNode{priv: priv, st: app.NewChatStates()}
		copy(c.pub[:], pub)
		c.node = convsync.NodeOf(c.pub)
		net.pubs[c.node] = c.pub
		c.port = &chatPort{n: net, self: c.node}
		kv := newKV()
		eng, err := convsync.New(convsync.Config{
			Pub: c.pub, Sign: func(b []byte) ([]byte, error) { return ed25519.Sign(c.priv, b), nil },
			Store: kv, IDs: ids, Net: c.st.Wrap(c.port), Dir: net,
			Now: func() time.Time { return testNow },
		})
		if err != nil {
			t.Fatal(err)
		}
		c.eng = eng
		c.rig = newRig(t, app.ModePublic)
		a, err := app.New(app.Config{
			Runtime: c.rig.rt, Links: c.rig.ln, Store: c.rig.kv, Clock: clock{}, Self: c.node, Mode: app.ModePublic, Version: "0.3.0",
			Convs: eng, States: c.st,
			PubOf: func(ctx context.Context, n [32]byte) ([32]byte, error) { return net.Pub(ctx, n) },
		})
		if err != nil {
			t.Fatal(err)
		}
		c.app = a
		net.nodes[c.node] = c
		out = append(out, c)
	}
	return net, out[0], out[1]
}

// befriend adds the other node to c's contacts.
func (c *chatNode) befriend(t *testing.T, o *chatNode) string {
	t.Helper()
	url := "echofog://join?v=2&card=" + hex.EncodeToString(o.node[:4])
	c.rig.ln.preview[url] = &links.Preview{Kind: links.KindContact, Node: o.node, Device: o.node, NotAfter: testNow.Add(time.Hour)}
	c.rig.ln.peer = node.Peer{Node: node.NodeID(o.node), Device: node.DeviceID(o.node)}
	got, err := c.app.AddByLink(context.Background(), url)
	if err != nil {
		t.Fatalf("AddByLink: %v", err)
	}
	return got.Node
}

func dm(nodeHex string) app.ChatRef { return app.ChatRef{Kind: "dm", ID: nodeHex} }

func (c *chatNode) nodeHex() string { return hex.EncodeToString(c.node[:]) }

// ---- tests -----------------------------------------------------------------------------------------------------

func TestDirectMessageArrivesAndShowsInTheChatList(t *testing.T) {
	net, a, b := newChatPair(t)
	a.befriend(t, b)
	b.befriend(t, a)
	ctx := context.Background()

	m, err := a.app.ChatSend(ctx, dm(b.nodeHex()), app.SendReq{Text: "привет"})
	if err != nil {
		t.Fatalf("ChatSend: %v", err)
	}
	if !m.Mine || m.Text != "привет" {
		t.Fatalf("sent message %+v", m)
	}
	net.drain(t)

	page, err := b.app.ChatHistory(ctx, dm(a.nodeHex()), "", 20)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Text != "привет" || page.Messages[0].Mine {
		t.Fatalf("history at b: %+v, %v", page, err)
	}
	list, err := b.app.Conversations()
	if err != nil || len(list) != 1 {
		t.Fatalf("conversations %+v, %v", list, err)
	}
	e := list[0]
	if e.Ref != dm(a.nodeHex()) || e.Unread != 1 || e.Request || e.Kind != "direct" || e.Last == nil || e.Last.Text != "привет" {
		t.Fatalf("entry %+v", e)
	}
}

func TestChatWithAStrangerIsAMessageRequest(t *testing.T) {
	net, a, b := newChatPair(t)
	a.befriend(t, b) // b did not add a
	if _, err := a.app.ChatSend(context.Background(), dm(b.nodeHex()), app.SendReq{Text: "здравствуйте"}); err != nil {
		t.Fatal(err)
	}
	net.drain(t)
	list, err := b.app.Conversations()
	if err != nil || len(list) != 1 || !list[0].Request {
		t.Fatalf("a stranger's chat must be a request: %+v, %v", list, err)
	}
}

func TestEditDeleteReactAndPinReachThePeer(t *testing.T) {
	net, a, b := newChatPair(t)
	a.befriend(t, b)
	b.befriend(t, a)
	ctx := context.Background()
	ref := dm(b.nodeHex())
	m, err := a.app.ChatSend(ctx, ref, app.SendReq{Text: "раз"})
	if err != nil {
		t.Fatal(err)
	}
	net.drain(t)
	atB := dm(a.nodeHex())

	if err := a.app.ChatEdit(ctx, ref, m.ID, "два"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := b.app.ChatReact(ctx, atB, m.ID, "👍", true); err != nil {
		t.Fatalf("react: %v", err)
	}
	if err := b.app.ChatPin(ctx, atB, m.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	net.drain(t)
	got, err := a.app.ChatMessage(ctx, ref, m.ID)
	if err != nil || got.Text != "два" || !got.Edited || !got.Pinned || len(got.Reactions) != 1 {
		t.Fatalf("at a: %+v, %v", got, err)
	}
	pins, err := b.app.ChatPins(ctx, atB)
	if err != nil || len(pins) != 1 || pins[0].ID != m.ID {
		t.Fatalf("pins: %+v, %v", pins, err)
	}

	if err := a.app.ChatDelete(ctx, ref, m.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	net.drain(t)
	if got, _ := b.app.ChatMessage(ctx, atB, m.ID); !got.Deleted {
		t.Fatalf("not deleted at b: %+v", got)
	}
}

func TestOnlyTheAuthorEditsAMessage(t *testing.T) {
	net, a, b := newChatPair(t)
	a.befriend(t, b)
	b.befriend(t, a)
	ctx := context.Background()
	m, _ := a.app.ChatSend(ctx, dm(b.nodeHex()), app.SendReq{Text: "моё"})
	net.drain(t)
	if err := b.app.ChatEdit(ctx, dm(a.nodeHex()), m.ID, "подмена"); err == nil {
		t.Fatal("a peer edited somebody else's message")
	}
}

func TestRepliesAndThreads(t *testing.T) {
	net, a, b := newChatPair(t)
	a.befriend(t, b)
	b.befriend(t, a)
	ctx := context.Background()
	root, _ := a.app.ChatSend(ctx, dm(b.nodeHex()), app.SendReq{Text: "корень"})
	net.drain(t)
	reply, err := b.app.ChatSend(ctx, dm(a.nodeHex()), app.SendReq{Text: "ответ", ReplyTo: root.ID})
	if err != nil || reply.ReplyTo != root.ID {
		t.Fatalf("reply %+v, %v", reply, err)
	}
	c, err := b.app.ChatSend(ctx, dm(a.nodeHex()), app.SendReq{Text: "в ветке", Thread: root.ID})
	if err != nil || c.Thread != root.ID {
		t.Fatalf("comment %+v, %v", c, err)
	}
	net.drain(t)
	th, err := a.app.ChatThread(ctx, dm(b.nodeHex()), root.ID, "", 20)
	if err != nil || len(th.Messages) != 1 || th.Messages[0].Text != "в ветке" {
		t.Fatalf("thread: %+v, %v", th, err)
	}
	page, _ := a.app.ChatHistory(ctx, dm(b.nodeHex()), "", 20)
	for _, m := range page.Messages {
		if m.Text == "в ветке" {
			t.Fatal("a comment shows in the main history")
		}
	}
}

func TestMarkReadClearsTheUnreadCount(t *testing.T) {
	net, a, b := newChatPair(t)
	a.befriend(t, b)
	b.befriend(t, a)
	ctx := context.Background()
	var last convlog.Message
	for _, s := range []string{"раз", "два", "три"} {
		last, _ = a.app.ChatSend(ctx, dm(b.nodeHex()), app.SendReq{Text: s})
	}
	net.drain(t)
	if l, _ := b.app.Conversations(); len(l) != 1 || l[0].Unread != 3 {
		t.Fatalf("unread before: %+v", l)
	}
	if err := b.app.ChatMarkRead(ctx, dm(a.nodeHex()), last.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := b.app.Conversations(); l[0].Unread != 0 {
		t.Fatalf("unread after: %+v", l)
	}
}

func TestDeliveryStatesAreHonest(t *testing.T) {
	net, a, b := newChatPair(t)
	a.befriend(t, b)
	b.befriend(t, a)
	ctx := context.Background()
	m, err := a.app.ChatSend(ctx, dm(b.nodeHex()), app.SendReq{Text: "жду"})
	if err != nil {
		t.Fatal(err)
	}
	if m.State == app.StateDelivered || m.State == "" {
		t.Fatalf("a message nobody confirmed shows %q", m.State)
	}
	id := a.port.last
	for _, c := range []struct {
		st   chat.State
		want string
	}{
		{chat.StateWaiting, app.StateOffline},
		{chat.StateMailbox, app.StateRelayed},
		{chat.StateFailed, app.StateExpired},
		{chat.StateDelivered, app.StateDelivered},
	} {
		a.st.OnStatus(id, c.st)
		got, _ := a.app.ChatMessage(ctx, dm(b.nodeHex()), m.ID)
		if got.State != c.want {
			t.Fatalf("after %v: state %q, want %q", c.st, got.State, c.want)
		}
	}
	net.drain(t)
	// the peer's own copy of the message has no delivery state: it is not mine
	if got, _ := b.app.ChatMessage(ctx, dm(a.nodeHex()), m.ID); got.State != "" {
		t.Fatalf("state on a received message: %q", got.State)
	}
}

func TestUnknownChatsAndBadIDsAreRefused(t *testing.T) {
	_, a, b := newChatPair(t)
	a.befriend(t, b)
	ctx := context.Background()
	if _, err := a.app.ChatHistory(ctx, app.ChatRef{Kind: "dm", ID: "zz"}, "", 10); !errors.Is(err, app.ErrNoChat) {
		t.Fatalf("bad node: %v", err)
	}
	if _, err := a.app.ChatHistory(ctx, dm(hex.EncodeToString(make([]byte, 32))), "", 10); !errors.Is(err, app.ErrNoChat) {
		t.Fatalf("stranger node: %v", err)
	}
	if _, err := a.app.ChatHistory(ctx, app.ChatRef{Kind: "weird", ID: b.nodeHex()}, "", 10); !errors.Is(err, app.ErrNoChat) {
		t.Fatalf("bad kind: %v", err)
	}
	if err := a.app.ChatEdit(ctx, dm(b.nodeHex()), "nothex", "x"); err == nil {
		t.Fatal("a bad message id was accepted")
	}
	if _, err := a.app.ChatSend(ctx, dm(b.nodeHex()), app.SendReq{Text: ""}); err == nil {
		t.Fatal("an empty message was sent")
	}
}

func TestBlockedContactGetsNoMessages(t *testing.T) {
	net, a, b := newChatPair(t)
	n := a.befriend(t, b)
	if err := a.app.Block(n); err != nil {
		t.Fatal(err)
	}
	if _, err := a.app.ChatSend(context.Background(), dm(b.nodeHex()), app.SendReq{Text: "тебе"}); !errors.Is(err, app.ErrBlocked) {
		t.Fatalf("send to a blocked contact: %v", err)
	}
	net.drain(t)
	if l, _ := b.app.Conversations(); len(l) != 0 {
		t.Fatalf("b heard from a blocked sender: %+v", l)
	}
}

func TestSubscribersHearOfChanges(t *testing.T) {
	net, a, b := newChatPair(t)
	a.befriend(t, b)
	b.befriend(t, a)
	var mu sync.Mutex
	var got []app.ConvChange
	cancel := b.app.SubscribeChats(func(c app.ConvChange) { mu.Lock(); got = append(got, c); mu.Unlock() })
	if _, err := a.app.ChatSend(context.Background(), dm(b.nodeHex()), app.SendReq{Text: "слышно?"}); err != nil {
		t.Fatal(err)
	}
	net.drain(t)
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n == 0 || got[0].Ref != dm(a.nodeHex()) || len(got[0].Messages) != 1 || got[0].Conversation.Unread != 1 || got[0].Rev == 0 {
		t.Fatalf("changes: %+v", got)
	}
	cancel()
	if _, err := a.app.ChatSend(context.Background(), dm(b.nodeHex()), app.SendReq{Text: "ещё"}); err != nil {
		t.Fatal(err)
	}
	net.drain(t)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != n {
		t.Fatal("a cancelled subscriber was called")
	}
}

func TestChatCallsWithoutAnEngineAreRefused(t *testing.T) {
	r := newRig(t, app.ModePublic)
	if _, err := r.a.Conversations(); !errors.Is(err, app.ErrChatOff) {
		t.Fatalf("Conversations: %v", err)
	}
	if _, err := r.a.ChatSend(context.Background(), dm("00"), app.SendReq{Text: "x"}); !errors.Is(err, app.ErrChatOff) {
		t.Fatalf("ChatSend: %v", err)
	}
}
