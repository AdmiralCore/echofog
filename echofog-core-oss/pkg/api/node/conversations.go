package node

import (
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/dto"
)

// Conversations: chats kept as conversation logs (internal/services/convlog). Every message and
// every operation on one — edit, delete, reaction, pin, reply, comment in a thread — is a signed
// event that syncs between the participants. See docs/design/conversation-log.md.

// ChatMessage is a message as a chat shows it: every operation on it resolved (the text after
// edits, deleted, reactions, pin, replies in its thread) and, for my own, its delivery state.
type ChatMessage = convlog.Message

// ChatReaction is one emoji on a message and who set it.
type ChatReaction = convlog.Reaction

// ChatRef names a chat: a direct chat with a peer (Kind "dm", ID its peer id hex) or a group (Kind
// "group", ID the group id).
type ChatRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ConversationDTO is one entry of the chat list.
type ConversationDTO struct {
	Ref     ChatRef      `json:"ref"`
	Conv    string       `json:"conv"`
	Title   string       `json:"title,omitempty"` // a group's title (a direct chat is named by the contact)
	Last    *ChatMessage `json:"last,omitempty"`
	LastTS  int64        `json:"lastTs"` // ms
	Unread  int          `json:"unread"`
	Request bool         `json:"request"` // a stranger's chat: shown among the message requests
	// Upgraded: the chat runs on the log, so replies, threads, edits, deletes, reactions and pins
	// work. A group runs on it once every member's app does; until then only plain messages.
	Upgraded bool `json:"upgraded"`
}

// ChatPage is one page of a chat or a thread; Next is the cursor of the following page, "" at the
// end.
type ChatPage struct {
	Messages []ChatMessage `json:"messages"`
	Next     string        `json:"next"`
}

// SendRequest is a message to send.
type SendRequest struct {
	Text    string `json:"text"`
	ReplyTo string `json:"replyTo,omitempty"` // the message answered
	Thread  string `json:"thread,omitempty"`  // the root message of the thread (a comment)
}

// ConvChange is pushed to the app when a chat changes: the chat-list entry and the messages that
// are new or changed.
type ConvChange struct {
	Ref          ChatRef         `json:"ref"`
	Conversation ConversationDTO `json:"conversation"`
	Messages     []ChatMessage   `json:"messages"`
}

var (
	// ErrNeedsUpgrade: the group does not run on the log yet (a member's app is older), so only
	// plain messages work in it.
	ErrNeedsUpgrade = errors.New("not every member of this group has updated the app yet")
	errBadChat      = errors.New("unknown chat")
)

// stateDelivered: my message, confirmed by the recipient (it left the outbox).
const stateDelivered = "delivered"

type convsHolder struct {
	mu   sync.Mutex
	f    *convsFacade
	subs []func(ConvChange)
}

type convsFacade struct {
	n      *NodeApi
	log    *convlog.Log
	broker *messenger.MessageBroker
	st     storage.Storage
	self   types.PeerID

	mu        sync.Mutex
	capable   map[types.PeerID]bool
	lastHello map[types.PeerID]time.Time

	migMu sync.Mutex // one migration at a time
}

// helloEvery is the least time between two sync openings with one peer while it stays connected;
// helloAgain is when a new connection's opening is repeated. The first frames on a fresh connection
// can be lost (the two sides dialled each other and one connection replaced the other), and a sync
// message is sent once, untracked: without the repeat, what the peer missed (a reaction, a pin) would
// wait for the next connection.
const (
	helloEvery = 30 * time.Second
	helloAgain = 8 * time.Second
)

func (n *NodeApi) convsFacade() (*convsFacade, error) {
	n.convsH.mu.Lock()
	defer n.convsH.mu.Unlock()
	if n.convsH.f != nil {
		return n.convsH.f, nil
	}
	if n.node == nil || n.node.Storage == nil || n.node.Messenger == nil {
		return nil, errNoChat
	}
	pub, err := n.node.IdentityPublicKey()
	if err != nil || len(pub) != 32 {
		return nil, errors.New("identity not available")
	}
	f := &convsFacade{n: n, broker: n.node.Messenger, st: n.node.Storage, self: n.node.GetSelfPeerID(),
		capable: map[types.PeerID]bool{}, lastHello: map[types.PeerID]time.Time{}}
	f.log = convlog.New(n.node.Storage, pub, n.node.SignIdentity)
	f.log.SetGroupPolicy(groupPolicy{n: n})
	f.log.OnChange(f.onChange)
	n.node.Messenger.SetEventSink(f.sink)
	n.node.Messenger.AddHandleCallback(f.onPlain)
	if n.node.Swarm != nil {
		n.node.Swarm.AddObserver(f)
	}
	n.convsH.f = f
	go f.migrate()
	go f.antiEntropy()
	return f, nil
}

// SubscribeConversations calls cb whenever a chat changes.
func (n *NodeApi) SubscribeConversations(cb func(ConvChange)) {
	n.convsH.mu.Lock()
	n.convsH.subs = append(n.convsH.subs, cb)
	n.convsH.mu.Unlock()
}

func (n *NodeApi) convRef(ref ChatRef) (convlog.Conv, error) {
	switch ref.Kind {
	case convlog.KindDM:
		raw, err := hex.DecodeString(ref.ID)
		if err != nil || len(raw) != len(types.PeerID{}) {
			return convlog.Conv{}, errBadChat
		}
		var peer types.PeerID
		copy(peer[:], raw)
		return convlog.DM(n.node.GetSelfPeerID(), peer), nil
	case convlog.KindGroup:
		gf, err := n.groupsFacade()
		if err != nil || !gf.isMember(ref.ID, n.node.GetSelfPeerID()) {
			return convlog.Conv{}, errBadChat
		}
		return convlog.Group(ref.ID), nil
	}
	return convlog.Conv{}, errBadChat
}

func refOf(c convlog.Conv) ChatRef {
	if c.Kind == convlog.KindGroup {
		return ChatRef{Kind: convlog.KindGroup, ID: c.Group}
	}
	return ChatRef{Kind: convlog.KindDM, ID: hex.EncodeToString(c.Peer[:])}
}

func parseRef(s string) (convlog.EventID, error) {
	if s == "" {
		return convlog.EventID{}, nil
	}
	return convlog.ParseEventID(s)
}

// --- capability: who speaks the log --------------------------------------------------------

func (f *convsFacade) capKey(peer types.PeerID) []byte {
	return []byte("cl/x/cap/" + storage.BlindID(f.st, hex.EncodeToString(peer[:])))
}

// markCapable remembers that the peer speaks the log (it sent us log traffic).
func (f *convsFacade) markCapable(peer types.PeerID) {
	f.mu.Lock()
	known := f.capable[peer]
	f.capable[peer] = true
	f.mu.Unlock()
	if !known && !f.st.Exists(f.capKey(peer)) {
		_ = f.st.Set(f.capKey(peer), []byte{1})
	}
}

func (f *convsFacade) isCapable(peer types.PeerID) bool {
	f.mu.Lock()
	ok := f.capable[peer]
	f.mu.Unlock()
	if ok {
		return true
	}
	if f.st.Exists(f.capKey(peer)) {
		f.mu.Lock()
		f.capable[peer] = true
		f.mu.Unlock()
		return true
	}
	return false
}

// --- receiving -------------------------------------------------------------------------------

// sink takes the log traffic the messenger received; see messenger.EventSink.
func (f *convsFacade) sink(origin types.PeerID, msg *messenger.PersonalMessageProto, _ bool) bool {
	f.markCapable(origin)
	if len(msg.ConvSync) > 0 {
		m, err := convlog.DecodeSync(msg.ConvSync)
		if err != nil {
			return false
		}
		go func() {
			// events synced into a group: its listeners reload, as after the old history handover
			group, before := f.syncTarget(m)
			for _, r := range f.log.HandleSync(origin, m, f.groupConv) {
				f.sendSync(origin, r)
			}
			if group != "" {
				if meta, ok := f.log.Meta(convlog.Group(group)); ok && string(meta.Digest()) != string(before) {
					f.groupHistoryArrived(group)
				}
			}
		}()
		return false
	}
	c, ok := f.convOfEvent(origin, msg.ConvEvent)
	if !ok {
		return false
	}
	id, fresh, err := f.log.Ingest(c, msg.ConvEvent)
	if err != nil {
		f.n.node.Logger.Debug("Conversations: event refused", "peer", origin.String()[:8], "err", err)
		return false
	}
	kind, _ := convlog.Peek(msg.ConvEvent)
	if c.Kind == convlog.KindGroup {
		// a group event in a member's own session (the group's epoch was not clean): the group's
		// listeners hear of a message, the direct chat's do not
		if fresh && kind == convlog.KindMsg {
			f.groupMessageArrived(c.Group, origin, id)
		}
		return false
	}
	return fresh && kind == convlog.KindMsg
}

// convOfEvent finds the conversation an event a peer sent us belongs to: our direct chat with it,
// or a group we share with it.
func (f *convsFacade) convOfEvent(origin types.PeerID, raw []byte) (convlog.Conv, bool) {
	id, ok := convlog.PeekConv(raw)
	if !ok {
		return convlog.Conv{}, false
	}
	if dm := convlog.DM(f.self, origin); dm.ID == id {
		return dm, true
	}
	c, ok := f.groupConv(id)
	if !ok {
		return convlog.Conv{}, false
	}
	if gf, err := f.n.groupsFacade(); err != nil || !gf.isMember(c.Group, origin) {
		return convlog.Conv{}, false
	}
	return c, true
}

// onPlain mirrors a message that came or went the old way (a file message, a peer that has not
// upgraded, history sent before the log) into the log, so the chat shows everything in one place.
func (f *convsFacade) onPlain(m *dto.MessageDTO) {
	peerHex := m.SenderID
	if m.IsMine {
		peerHex = m.TargetID
	}
	raw, err := hex.DecodeString(peerHex)
	if err != nil || len(raw) != len(types.PeerID{}) {
		return
	}
	var peer types.PeerID
	copy(peer[:], raw)
	c := convlog.DM(f.self, peer)
	if id, err := convlog.ParseEventID(m.ID); err == nil && f.log.HasMessage(c, id) {
		return // a log message, already in place
	}
	author := peer
	if m.IsMine {
		author = f.self
	}
	_, _ = f.log.AddLegacy(c, []convlog.Legacy{{ID: m.ID, Author: author, TS: m.Timestamp / int64(time.Millisecond),
		Text: m.Text, Type: uint32(m.Type), Metadata: m.Metadata, ReplyTo: m.ReplyToID}}, true)
}

// OnPeerConnected opens a sync of our direct chat with a peer that speaks the log.
func (f *convsFacade) OnPeerConnected(peer types.PeerID, _ types.PeerPublicKey, _ string) {
	if !f.isCapable(peer) {
		return
	}
	f.mu.Lock()
	if time.Since(f.lastHello[peer]) < helloEvery {
		f.mu.Unlock()
		return
	}
	f.lastHello[peer] = time.Now()
	f.mu.Unlock()
	go f.sendSync(peer, f.log.Hello(f.sharedConvs(peer)))
	time.AfterFunc(helloAgain, func() {
		if f.n.node.Ctx.Err() == nil && f.n.node.Swarm.GetPeer(peer) != nil {
			f.sendSync(peer, f.log.Hello(f.sharedConvs(peer)))
		}
	})
}

// OnPeerDisconnected lets the next connection open a sync at once.
func (f *convsFacade) OnPeerDisconnected(peer types.PeerID) {
	f.mu.Lock()
	delete(f.lastHello, peer)
	f.mu.Unlock()
}

// --- sending ---------------------------------------------------------------------------------

func (f *convsFacade) sendSync(peer types.PeerID, m *convlog.Sync) {
	raw, err := convlog.EncodeSync(m)
	if err != nil {
		return
	}
	_, _ = f.broker.SendEvent(peer, "", messenger.SendOptions{ConvSync: raw, NoTrack: true, DirectOnly: true})
}

// sendEvent hands an event of ours to the other participant. A message always goes (with its text,
// for a node that has not upgraded); an operation only to a peer that speaks the log — any other
// gets it through sync once it does.
func (f *convsFacade) sendEvent(c convlog.Conv, raw []byte, id convlog.EventID, kind uint32, text string, replyTo string) (string, error) {
	if c.Kind == convlog.KindGroup {
		gf, err := f.n.groupsFacade()
		if err != nil {
			return "", err
		}
		track := kind == convlog.KindMsg || kind == convlog.KindEdit || kind == convlog.KindDelete
		gf.postEvent(c.Group, raw, track, text)
		return "", nil
	}
	peer := c.Peer
	opts := messenger.SendOptions{ID: id.String(), ConvEvent: raw}
	switch kind {
	case convlog.KindMsg:
		opts.ReplyToID = replyTo
		return f.broker.SendEvent(peer, text, opts)
	case convlog.KindEdit, convlog.KindDelete:
		if !f.isCapable(peer) {
			return "", nil
		}
		return f.broker.SendEvent(peer, "", opts)
	default:
		if !f.isCapable(peer) {
			return "", nil
		}
		opts.NoTrack = true
		return f.broker.SendEvent(peer, "", opts)
	}
}

// --- what the app sees -----------------------------------------------------------------------

// withStates fills in the delivery state of my messages in c.
func (f *convsFacade) withStates(c convlog.Conv, msgs []convlog.Message) []convlog.Message {
	if c.Kind != convlog.KindDM {
		return msgs
	}
	pending := f.broker.PendingStates(c.Peer)
	for i := range msgs {
		if !msgs[i].Mine || msgs[i].Legacy {
			continue
		}
		if s, ok := pending[msgs[i].ID]; ok {
			msgs[i].State = s
		} else {
			msgs[i].State = stateDelivered
		}
	}
	return msgs
}

func (f *convsFacade) conversation(c convlog.Conv, meta convlog.Meta) ConversationDTO {
	d := ConversationDTO{Ref: refOf(c), Conv: meta.Conv, LastTS: meta.LastTS, Unread: meta.Unread}
	if meta.LastID != "" {
		if id, err := convlog.ParseEventID(meta.LastID); err == nil {
			if m, ok := f.log.Message(c, id); ok {
				m = f.withStates(c, []convlog.Message{m})[0]
				d.Last = &m
			}
		}
	}
	switch c.Kind {
	case convlog.KindDM:
		if f.n.node != nil {
			d.Request = !f.n.node.IsContact(c.Peer)
		}
		d.Upgraded = f.isCapable(c.Peer)
	case convlog.KindGroup:
		if gf, err := f.n.groupsFacade(); err == nil {
			d.Title = gf.titleOf(c.Group)
			d.Upgraded = gf.LogMode(c.Group)
		}
	}
	return d
}

func (f *convsFacade) onChange(ch convlog.Change) {
	out := ConvChange{Ref: refOf(ch.Conv), Conversation: f.conversation(ch.Conv, ch.Meta),
		Messages: f.withStates(ch.Conv, ch.Messages)}
	f.n.convsH.mu.Lock()
	subs := append(([]func(ConvChange))(nil), f.n.convsH.subs...)
	f.n.convsH.mu.Unlock()
	for _, s := range subs {
		s(out)
	}
}

// --- the history from before the log --------------------------------------------------------

const migratedKey = "cl/x/migrated-v1"

// migrate brings the direct chats kept before the log into it, once. Reads of the history wait
// for it, so a chat never shows without its past.
func (f *convsFacade) migrate() {
	if f.st.Exists([]byte(migratedKey)) {
		return
	}
	f.migMu.Lock()
	defer f.migMu.Unlock()
	if f.st.Exists([]byte(migratedKey)) {
		return
	}
	var peers []string
	for attempt := 0; ; attempt++ {
		cs, err := f.n.GetContacts()
		if err == nil {
			for _, c := range cs {
				peers = append(peers, c.PeerID)
			}
			break
		}
		if attempt > 30 {
			return // contacts never came up; try again next start
		}
		time.Sleep(time.Second)
	}
	if reqs, err := f.n.GetMessageRequests(); err == nil {
		for _, r := range reqs {
			peers = append(peers, r.PeerID)
		}
	}
	myID := hex.EncodeToString(f.self[:])
	for _, p := range peers {
		raw, err := hex.DecodeString(p)
		if err != nil || len(raw) != len(types.PeerID{}) {
			continue
		}
		var peer types.PeerID
		copy(peer[:], raw)
		msgs, err := f.broker.GetKeeper().Getter.GetMessages(0, messenger.GenerateLocalChatID(myID, p, false), 1<<20, 0, myID, p)
		if err != nil {
			continue
		}
		old := make([]convlog.Legacy, 0, len(msgs))
		for _, m := range msgs {
			author := peer
			if m.IsMine {
				author = f.self
			}
			old = append(old, convlog.Legacy{ID: m.ID, Author: author, TS: m.Timestamp / int64(time.Millisecond),
				Text: m.Text, Type: uint32(m.Type), Metadata: m.Metadata, ReplyTo: m.ReplyToID})
		}
		if _, err := f.log.AddLegacy(convlog.DM(f.self, peer), old, false); err != nil {
			return // try again next start
		}
	}
	_ = f.st.Set([]byte(migratedKey), []byte{1})
}

// --- API -------------------------------------------------------------------------------------

// Conversations lists the chats, most recently active first.
func (n *NodeApi) Conversations() ([]ConversationDTO, error) {
	f, err := n.convsFacade()
	if err != nil {
		return nil, err
	}
	f.migrate()
	f.migrateGroups()
	metas, err := f.log.Conversations()
	if err != nil {
		return nil, err
	}
	out := make([]ConversationDTO, 0, len(metas))
	for _, m := range metas {
		c, ok := convOfMeta(m)
		if !ok {
			continue
		}
		out = append(out, f.conversation(c, m))
	}
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

func (n *NodeApi) chat(ref ChatRef) (*convsFacade, convlog.Conv, error) {
	f, err := n.convsFacade()
	if err != nil {
		return nil, convlog.Conv{}, err
	}
	f.migrate()
	f.migrateGroups()
	c, err := n.convRef(ref)
	return f, c, err
}

// ChatHistory returns a page of a chat, newest message first. Pass "" for the newest page and the
// returned Next for the one before it. Comments in threads are not in it (see ChatThread).
func (n *NodeApi) ChatHistory(ref ChatRef, cursor string, limit int) (ChatPage, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return ChatPage{}, err
	}
	msgs, next, err := f.log.History(c, cursor, limit)
	if err != nil {
		return ChatPage{}, err
	}
	return ChatPage{Messages: f.withStates(c, msgs), Next: next}, nil
}

// ChatThread returns a page of the thread under root, oldest first.
func (n *NodeApi) ChatThread(ref ChatRef, root, cursor string, limit int) (ChatPage, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return ChatPage{}, err
	}
	rid, err := convlog.ParseEventID(root)
	if err != nil {
		return ChatPage{}, err
	}
	msgs, next, err := f.log.Thread(c, rid, cursor, limit)
	if err != nil {
		return ChatPage{}, err
	}
	return ChatPage{Messages: f.withStates(c, msgs), Next: next}, nil
}

// ChatPins returns the pinned messages of a chat, most recently pinned first.
func (n *NodeApi) ChatPins(ref ChatRef) ([]ChatMessage, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return nil, err
	}
	msgs, err := f.log.Pins(c)
	return f.withStates(c, msgs), err
}

// ChatMessage returns one message.
func (n *NodeApi) ChatMessage(ref ChatRef, id string) (ChatMessage, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return convlog.Message{}, err
	}
	eid, err := convlog.ParseEventID(id)
	if err != nil {
		return convlog.Message{}, err
	}
	m, ok := f.log.Message(c, eid)
	if !ok {
		return convlog.Message{}, convlog.ErrNotFound
	}
	return f.withStates(c, []convlog.Message{m})[0], nil
}

// ChatSend sends a message: plain, an answer to another (ReplyTo) or a comment in a thread (Thread).
func (n *NodeApi) ChatSend(ref ChatRef, req SendRequest) (ChatMessage, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return convlog.Message{}, err
	}
	reply, err := parseRef(req.ReplyTo)
	if err != nil {
		return convlog.Message{}, err
	}
	thread, err := parseRef(req.Thread)
	if err != nil {
		return convlog.Message{}, err
	}
	if c.Kind == convlog.KindGroup {
		gf, err := n.groupsFacade()
		if err != nil {
			return convlog.Message{}, err
		}
		if !gf.LogMode(c.Group) {
			// the group still runs the old way: a plain message, shown in the chat all the same
			if req.ReplyTo != "" || req.Thread != "" {
				return convlog.Message{}, ErrNeedsUpgrade
			}
			d, err := gf.SendMessage(c.Group, req.Text)
			if err != nil {
				return convlog.Message{}, err
			}
			f.mirrorGroup(c.Group, d)
			m, _ := f.log.Message(c, convlog.LegacyID(d.ID))
			return m, nil
		}
	}
	raw, id, err := f.log.AppendMessage(c, req.Text, 0, "", reply, thread)
	if err != nil {
		return convlog.Message{}, err
	}
	state, sendErr := f.sendEvent(c, raw, id, convlog.KindMsg, req.Text, req.ReplyTo)
	m, _ := f.log.Message(c, id)
	if state != "" {
		m.State = state
	}
	// the listeners of plain messages (the current chat screen) hear of it as before
	if c.Kind == convlog.KindDM {
		f.broker.StartCallbackHandlers(&dto.MessageDTO{ID: id.String(), IsMine: true, SenderID: hex.EncodeToString(f.self[:]),
			TargetID: hex.EncodeToString(c.Peer[:]), Text: req.Text, Timestamp: m.TS * int64(time.Millisecond),
			ReplyToID: req.ReplyTo, Pending: state != stateDelivered, State: state})
	}
	return m, sendErr
}

func (n *NodeApi) chatOp(ref ChatRef, target string, kind uint32, do func(*convlog.Log, convlog.Conv, convlog.EventID) ([]byte, convlog.EventID, error)) error {
	f, c, err := n.chat(ref)
	if err != nil {
		return err
	}
	if c.Kind == convlog.KindGroup {
		if gf, err := n.groupsFacade(); err != nil || !gf.LogMode(c.Group) {
			return ErrNeedsUpgrade
		}
	}
	tid, err := convlog.ParseEventID(target)
	if err != nil {
		return err
	}
	raw, id, err := do(f.log, c, tid)
	if err != nil {
		return err
	}
	_, err = f.sendEvent(c, raw, id, kind, "", "")
	return err
}

// ChatEdit replaces the text of one of my messages.
func (n *NodeApi) ChatEdit(ref ChatRef, id, text string) error {
	return n.chatOp(ref, id, convlog.KindEdit, func(l *convlog.Log, c convlog.Conv, t convlog.EventID) ([]byte, convlog.EventID, error) {
		return l.AppendEdit(c, t, text)
	})
}

// ChatDelete deletes a message for everyone: mine, or anyone's for a group moderator.
func (n *NodeApi) ChatDelete(ref ChatRef, id string) error {
	return n.chatOp(ref, id, convlog.KindDelete, func(l *convlog.Log, c convlog.Conv, t convlog.EventID) ([]byte, convlog.EventID, error) {
		return l.AppendDelete(c, t)
	})
}

// ChatReact sets (on) or clears my reaction with emoji on a message.
func (n *NodeApi) ChatReact(ref ChatRef, id, emoji string, on bool) error {
	return n.chatOp(ref, id, convlog.KindReact, func(l *convlog.Log, c convlog.Conv, t convlog.EventID) ([]byte, convlog.EventID, error) {
		return l.AppendReact(c, t, emoji, on)
	})
}

// ChatPin pins (on) or unpins a message for the whole chat.
func (n *NodeApi) ChatPin(ref ChatRef, id string, on bool) error {
	return n.chatOp(ref, id, convlog.KindPin, func(l *convlog.Log, c convlog.Conv, t convlog.EventID) ([]byte, convlog.EventID, error) {
		return l.AppendPin(c, t, on)
	})
}

// ChatMarkRead records that the user has read the chat up to message id.
func (n *NodeApi) ChatMarkRead(ref ChatRef, id string) error {
	f, c, err := n.chat(ref)
	if err != nil {
		return err
	}
	eid, err := convlog.ParseEventID(id)
	if err != nil {
		return err
	}
	return f.log.MarkRead(c, eid)
}

// plainHistory serves the old GetMessages from the log: newest first, edits applied, deleted
// messages left out.
func (n *NodeApi) plainHistory(contactID string, limit int) ([]dto.MessageDTO, error) {
	page, err := n.ChatHistory(ChatRef{Kind: convlog.KindDM, ID: contactID}, "", limit)
	if err != nil {
		return nil, err
	}
	myID := n.GetMyID()
	out := make([]dto.MessageDTO, 0, len(page.Messages))
	for _, m := range page.Messages {
		if m.Deleted {
			continue
		}
		target := myID
		if m.Mine {
			target = contactID
		}
		out = append(out, scrubMessage(dto.MessageDTO{ID: m.ID, IsMine: m.Mine, SenderID: m.Author, TargetID: target,
			Type: dto.MessageType(m.Type), Text: m.Text, Metadata: m.Metadata, Timestamp: m.TS * int64(time.Millisecond),
			ReplyToID: m.ReplyTo, State: pendingOnly(m.State), Pending: pendingOnly(m.State) != ""}))
	}
	return out, nil
}

// pendingOnly is the old API's view of a state: "" once delivered.
func pendingOnly(s string) string {
	if s == stateDelivered {
		return ""
	}
	return s
}
