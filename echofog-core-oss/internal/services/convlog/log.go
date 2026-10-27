package convlog

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

var (
	ErrNotParticipant = errors.New("the author does not take part in this conversation")
	ErrForbidden      = errors.New("not allowed")
	ErrConvMismatch   = errors.New("the event belongs to another conversation")
	ErrClock          = errors.New("the event's clock jumps too far")
	ErrBadBody        = errors.New("bad event body")
	ErrNotFound       = errors.New("not found")
)

// maxEmojiPerMessage bounds the distinct reactions on one message.
const maxEmojiPerMessage = 64

// Policy decides who may do what in conversations of one kind.
type Policy interface {
	Participant(c Conv, author types.PeerID) bool
	CanPin(c Conv, author types.PeerID) bool
	CanModerate(c Conv, author types.PeerID) bool
}

type dmPolicy struct{ self types.PeerID }

func (p dmPolicy) Participant(c Conv, a types.PeerID) bool { return a == p.self || a == c.Peer }
func (p dmPolicy) CanPin(c Conv, a types.PeerID) bool      { return p.Participant(c, a) }
func (p dmPolicy) CanModerate(Conv, types.PeerID) bool     { return false }

// Change is what one applied event changed, for the app.
type Change struct {
	Conv     Conv
	Meta     Meta
	Messages []Message // messages whose state changed, resolved
}

// Log keeps the conversation logs of one account.
type Log struct {
	st      storage.Storage
	self    types.PeerID
	selfHex string
	selfPub []byte
	sign    func([]byte) ([]byte, error)
	now     func() time.Time

	mu          sync.Mutex
	groupPolicy atomic.Value // policyBox
	listeners   []func(Change)
}

type policyBox struct{ p Policy }

// New returns the log of the account with identity key selfPub; sign signs with it.
func New(st storage.Storage, selfPub []byte, sign func([]byte) ([]byte, error)) *Log {
	var pub types.PeerPublicKey
	copy(pub[:], selfPub)
	self := types.PeerPubKeyToID(pub)
	return &Log{st: st, self: self, selfHex: peerHex(self), selfPub: append([]byte(nil), selfPub...),
		sign: sign, now: time.Now}
}

// Self is this account's peer id.
func (l *Log) Self() types.PeerID { return l.self }

// SetGroupPolicy sets who may do what in groups (members, roles).
func (l *Log) SetGroupPolicy(p Policy) { l.groupPolicy.Store(policyBox{p}) }

// OnChange registers f to hear about every applied event.
func (l *Log) OnChange(f func(Change)) {
	l.mu.Lock()
	l.listeners = append(l.listeners, f)
	l.mu.Unlock()
}

func (l *Log) policy(c Conv) Policy {
	if c.Kind == KindGroup {
		if b, ok := l.groupPolicy.Load().(policyBox); ok && b.p != nil {
			return b.p
		}
	}
	if c.Kind == KindDM {
		return dmPolicy{self: l.self}
	}
	return denyAll{}
}

type denyAll struct{}

func (denyAll) Participant(Conv, types.PeerID) bool { return false }
func (denyAll) CanPin(Conv, types.PeerID) bool      { return false }
func (denyAll) CanModerate(Conv, types.PeerID) bool { return false }

func (l *Log) notify(ch *Change) {
	if ch == nil {
		return
	}
	l.mu.Lock()
	ls := append(([]func(Change))(nil), l.listeners...)
	l.mu.Unlock()
	for _, f := range ls {
		f(*ch)
	}
}

func (l *Log) loadMeta(bc string, c Conv) *Meta {
	k := metaKey(bc)
	if l.st.Exists(k) {
		if raw, err := l.st.Get(k); err == nil {
			var m Meta
			if json.Unmarshal(raw, &m) == nil {
				m.init()
				return &m
			}
		}
	}
	return newMeta(c)
}

// Open makes sure the conversation exists (so it shows in the list before its first message).
func (l *Log) Open(c Conv) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	bc := l.bc(c.ID)
	if l.st.Exists(metaKey(bc)) {
		return nil
	}
	raw, err := json.Marshal(newMeta(c))
	if err != nil {
		return err
	}
	return l.st.Set(metaKey(bc), raw)
}

// txn gathers the writes of one event so they land together.
type txn struct {
	l       *Log
	c       Conv
	bc      string
	meta    *Meta
	head    []storage.Entry // the event itself: written first
	sets    []storage.Entry
	dels    [][]byte
	views   map[string]*View
	order   []string
	changed map[string]bool
}

func (l *Log) newTxn(c Conv, bc string, meta *Meta) *txn {
	return &txn{l: l, c: c, bc: bc, meta: meta, views: map[string]*View{}, changed: map[string]bool{}}
}

func (t *txn) put(k, v []byte) { t.sets = append(t.sets, storage.Entry{Key: k, Value: v}) }
func (t *txn) del(k []byte)    { t.dels = append(t.dels, k) }
func (t *txn) touch(id string) { t.changed[id] = true }

func (t *txn) view(id string) *View {
	if v, ok := t.views[id]; ok {
		return v
	}
	v := &View{ID: id}
	k := viewKey(t.bc, id)
	if t.l.st.Exists(k) {
		if raw, err := t.l.st.Get(k); err == nil {
			_ = json.Unmarshal(raw, v)
			v.ID = id
		}
	}
	t.views[id] = v
	t.order = append(t.order, id)
	return v
}

func (t *txn) commit(marker []byte, id string) error {
	sets := append([]storage.Entry(nil), t.head...)
	sets = append(sets, t.sets...)
	for _, id := range t.order {
		raw, err := json.Marshal(t.views[id])
		if err != nil {
			return err
		}
		sets = append(sets, storage.Entry{Key: viewKey(t.bc, id), Value: raw})
	}
	if marker != nil {
		sets = append(sets, storage.Entry{Key: indexKey(t.bc, id), Value: marker})
	}
	meta, err := json.Marshal(t.meta)
	if err != nil {
		return err
	}
	sets = append(sets, storage.Entry{Key: metaKey(t.bc), Value: meta})
	return storage.WriteBatch(t.l.st, sets, t.dels)
}

func (t *txn) change() *Change {
	ch := &Change{Conv: t.c, Meta: *t.meta}
	ids := make([]string, 0, len(t.changed))
	for id := range t.changed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if v := t.views[id]; v != nil && v.Present {
			ch.Messages = append(ch.Messages, v.Render(t.meta.Conv, t.l.selfHex))
		}
	}
	return ch
}

type indexEntry struct {
	A string `json:"a"`
	S uint64 `json:"s"`
	F bool   `json:"f,omitempty"` // stored as a fork
}

func hexRef(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return hex.EncodeToString(b)
}

// strip rewrites a stored event without its body.
func (t *txn) strip(k []byte) {
	if !t.l.st.Exists(k) {
		return
	}
	raw, err := t.l.st.Get(k)
	if err != nil {
		return
	}
	var e Event
	if proto.Unmarshal(raw, &e) != nil || Stripped(&e) {
		return
	}
	if out, err := Encode(Strip(&e)); err == nil {
		t.put(k, out)
	}
}

func (t *txn) stripEdits(v *View) {
	for _, r := range v.EditRefs {
		t.strip(eventKey(t.bc, t.l.ba(r.A), r.S))
	}
	for k, r := range v.Edits {
		r.Text = ""
		v.Edits[k] = r
	}
}

// apply applies one verified event; store is what will be written for it (it may be stripped).
func (l *Log) apply(t *txn, e *Event, id EventID, raw []byte, own bool) error {
	authorID := AuthorID(e)
	author := peerHex(authorID)
	pol := l.policy(t.c)
	if !pol.Participant(t.c, authorID) {
		return ErrNotParticipant
	}
	if !own && e.Lamport > t.meta.MaxLamport+maxLamportJump {
		return ErrClock
	}
	idHex := id.String()
	ba := l.ba(author)
	ekey := eventKey(t.bc, ba, e.Seq)
	fork := l.st.Exists(ekey)
	store := raw

	var err error
	switch e.Kind {
	case KindMsg:
		err = t.applyMsg(e, idHex, author, ba, &store)
	case KindEdit:
		err = t.applyEdit(e, author, &store)
	case KindDelete:
		err = t.applyDelete(e, author, authorID, pol)
	case KindReact:
		err = t.applyReact(e, author)
	case KindPin:
		err = t.applyPin(e, author, authorID, pol)
	}
	if err != nil {
		return err
	}

	if fork {
		t.head = append(t.head, storage.Entry{Key: forkKey(t.bc, idHex), Value: store})
		if t.meta.Forks == nil {
			t.meta.Forks = map[string]uint64{}
		}
		t.meta.Forks[author] = e.Seq
	} else {
		t.head = append(t.head, storage.Entry{Key: ekey, Value: store})
		if e.Seq > t.meta.Max[author] {
			t.meta.Max[author] = e.Seq
		}
		if e.Seq == t.meta.VV[author]+1 {
			n := e.Seq
			for l.st.Exists(eventKey(t.bc, ba, n+1)) {
				n++
			}
			t.meta.VV[author] = n
		}
	}
	if e.Lamport > t.meta.MaxLamport {
		t.meta.MaxLamport = e.Lamport
	}
	if author == l.selfHex && e.Seq > t.meta.OwnSeq {
		t.meta.OwnSeq = e.Seq // ours, possibly handed back by a peer after a restore
	}
	return nil
}

func (t *txn) applyMsg(e *Event, idHex, author, ba string, store *[]byte) error {
	var body MsgBody
	stripped := Stripped(e)
	if !stripped {
		if err := proto.Unmarshal(e.Body, &body); err != nil || !validText(body.Text) || len(body.Metadata) > MaxTextLen {
			return ErrBadBody
		}
	}
	v := t.view(idHex)
	v.Present = true
	v.Author, v.Seq, v.Lamport, v.TS = author, e.Seq, e.Lamport, e.TsMs
	v.Type, v.Text, v.Metadata = body.Type, body.Text, body.Metadata
	v.ReplyTo, v.Thread = hexRef(e.ReplyTo), hexRef(e.Thread)
	v.Stripped = stripped
	del, _ := v.deleted()
	if del {
		if !stripped {
			if out, err := Encode(Strip(e)); err == nil {
				*store = out
			}
			v.Stripped = true
		}
		v.Text, v.Metadata = "", ""
		t.stripEdits(v)
	} else if stripped {
		v.Withheld = true
	}
	if v.Thread == "" {
		t.put(orderKey(t.bc, e.Lamport, idHex), []byte(idHex))
		if e.Lamport >= t.meta.LastLamport {
			t.meta.LastLamport, t.meta.LastID, t.meta.LastTS = e.Lamport, idHex, e.TsMs
		}
		if author == t.l.selfHex {
			if e.Lamport > t.meta.ReadLamport {
				t.meta.ReadLamport, t.meta.Unread = e.Lamport, 0
			}
		} else if e.Lamport > t.meta.ReadLamport {
			t.meta.Unread++
		}
	} else {
		t.put(threadKey(t.bc, v.Thread, e.Lamport, idHex), []byte(idHex))
		root := t.view(v.Thread)
		root.Replies++
		if e.TsMs > root.LastReplyTS {
			root.LastReplyTS = e.TsMs
		}
		t.touch(v.Thread)
	}
	if v.Pin != nil && v.Pin.On && !del {
		t.put(pinKey(t.bc, idHex), []byte(idHex))
	}
	t.touch(idHex)
	return nil
}

func (t *txn) applyEdit(e *Event, author string, store *[]byte) error {
	var body EditBody
	if !Stripped(e) {
		if err := proto.Unmarshal(e.Body, &body); err != nil || !validText(body.Text) {
			return ErrBadBody
		}
	}
	target := hexRef(e.Target)
	v := t.view(target)
	if v.Present && v.Author != author {
		return ErrForbidden
	}
	v.applyEdit(Reg{L: e.Lamport, A: author, S: e.Seq, TS: e.TsMs, Text: body.Text}, evRef{A: author, S: e.Seq})
	if del, _ := v.deleted(); del {
		if !Stripped(e) {
			if out, err := Encode(Strip(e)); err == nil {
				*store = out
			}
		}
		for k, r := range v.Edits {
			r.Text = ""
			v.Edits[k] = r
		}
	}
	t.touch(target)
	return nil
}

func (t *txn) applyDelete(e *Event, author string, authorID types.PeerID, pol Policy) error {
	target := hexRef(e.Target)
	v := t.view(target)
	mod := false
	if v.Present {
		if v.Author != author {
			if !pol.CanModerate(t.c, authorID) {
				return ErrForbidden
			}
			mod = true
		}
	} else if pol.CanModerate(t.c, authorID) {
		mod = true
	}
	v.applyDelete(Reg{L: e.Lamport, A: author, S: e.Seq, TS: e.TsMs, Mod: mod})
	if del, _ := v.deleted(); del && v.Present && !v.Stripped {
		t.strip(eventKey(t.bc, t.l.ba(v.Author), v.Seq))
		v.Stripped, v.Withheld = true, false
		v.Text, v.Metadata = "", ""
		t.stripEdits(v)
		if v.Pin != nil && v.Pin.On {
			t.del(pinKey(t.bc, target))
		}
	}
	t.touch(target)
	return nil
}

func (t *txn) applyReact(e *Event, author string) error {
	var body ReactBody
	if err := proto.Unmarshal(e.Body, &body); err != nil || !validEmoji(body.Emoji) {
		return ErrBadBody
	}
	target := hexRef(e.Target)
	v := t.view(target)
	if _, ok := v.Reacts[body.Emoji]; !ok && len(v.Reacts) >= maxEmojiPerMessage {
		return ErrForbidden
	}
	v.applyReact(body.Emoji, Reg{L: e.Lamport, A: author, S: e.Seq, TS: e.TsMs, On: body.On})
	t.touch(target)
	return nil
}

func (t *txn) applyPin(e *Event, author string, authorID types.PeerID, pol Policy) error {
	if !pol.CanPin(t.c, authorID) {
		return ErrForbidden
	}
	var body PinBody
	if err := proto.Unmarshal(e.Body, &body); err != nil {
		return ErrBadBody
	}
	target := hexRef(e.Target)
	v := t.view(target)
	if v.applyPin(Reg{L: e.Lamport, A: author, S: e.Seq, TS: e.TsMs, On: body.On}) {
		del, _ := v.deleted()
		if v.Pin.On && v.Present && !del {
			t.put(pinKey(t.bc, target), []byte(target))
		} else {
			t.del(pinKey(t.bc, target))
		}
	}
	t.touch(target)
	return nil
}

// Ingest applies an event that came from the network. It reports false for one already held.
func (l *Log) Ingest(c Conv, raw []byte) (EventID, bool, error) {
	e, id, err := Decode(raw)
	if err != nil {
		return EventID{}, false, err
	}
	if !bytes.Equal(e.Conv, c.ID[:]) {
		return id, false, ErrConvMismatch
	}
	l.mu.Lock()
	bc := l.bc(c.ID)
	if l.st.Exists(indexKey(bc, id.String())) {
		ch := l.upgradeWithheld(c, bc, e, id, raw)
		l.mu.Unlock()
		l.notify(ch)
		return id, false, nil
	}
	t := l.newTxn(c, bc, l.loadMeta(bc, c))
	if err := l.apply(t, e, id, raw, false); err != nil {
		l.mu.Unlock()
		return id, false, err
	}
	err = t.commit(l.marker(e), id.String())
	var ch *Change
	if err == nil {
		ch = t.change()
	}
	l.mu.Unlock()
	l.notify(ch)
	return id, err == nil, err
}

func (l *Log) marker(e *Event) []byte {
	raw, _ := json.Marshal(indexEntry{A: peerHex(AuthorID(e)), S: e.Seq})
	return raw
}

// upgradeWithheld takes the full copy of a message that first arrived without its body, unless
// it has been deleted since.
func (l *Log) upgradeWithheld(c Conv, bc string, e *Event, id EventID, raw []byte) *Change {
	if e.Kind != KindMsg || Stripped(e) {
		return nil
	}
	meta := l.loadMeta(bc, c)
	t := l.newTxn(c, bc, meta)
	v := t.view(id.String())
	if !v.Withheld {
		return nil
	}
	if del, _ := v.deleted(); del {
		return nil
	}
	var body MsgBody
	if proto.Unmarshal(e.Body, &body) != nil || !validText(body.Text) {
		return nil
	}
	v.Text, v.Type, v.Metadata, v.Withheld, v.Stripped = body.Text, body.Type, body.Metadata, false, false
	t.head = append(t.head, storage.Entry{Key: eventKey(bc, l.ba(v.Author), v.Seq), Value: raw})
	t.touch(v.ID)
	if t.commit(nil, "") != nil {
		return nil
	}
	return t.change()
}

// Draft is an event this account is about to write.
type Draft struct {
	Kind    uint32
	Target  EventID
	ReplyTo EventID
	Thread  EventID
	Body    []byte
}

// Append writes an event of this account and returns its encoded form, to send to the others.
func (l *Log) Append(c Conv, d Draft) ([]byte, EventID, error) {
	l.mu.Lock()
	bc := l.bc(c.ID)
	meta := l.loadMeta(bc, c)
	seq := meta.OwnSeq + 1
	if m := meta.Max[l.selfHex]; m >= seq {
		seq = m + 1
	}
	e := &Event{Conv: c.ID[:], Author: l.selfPub, Seq: seq, Lamport: meta.MaxLamport + 1,
		TsMs: l.now().UnixMilli(), Kind: d.Kind, Body: d.Body}
	if !d.Target.IsZero() {
		e.Target = d.Target[:]
	}
	if !d.ReplyTo.IsZero() {
		e.ReplyTo = d.ReplyTo[:]
	}
	if !d.Thread.IsZero() {
		e.Thread = d.Thread[:]
	}
	if err := Sign(e, l.sign); err != nil {
		l.mu.Unlock()
		return nil, EventID{}, err
	}
	raw, err := Encode(e)
	if err != nil {
		l.mu.Unlock()
		return nil, EventID{}, err
	}
	id := idOf(e)
	t := l.newTxn(c, bc, meta)
	if err := l.apply(t, e, id, raw, true); err != nil {
		l.mu.Unlock()
		return nil, id, err
	}
	err = t.commit(l.marker(e), id.String())
	var ch *Change
	if err == nil {
		ch = t.change()
	}
	l.mu.Unlock()
	l.notify(ch)
	if err != nil {
		return nil, id, err
	}
	return raw, id, nil
}

func encodeBody(m proto.Message) []byte {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	return b
}

// AppendMessage writes a message. replyTo and thread may be zero.
func (l *Log) AppendMessage(c Conv, text string, typ uint32, metadata string, replyTo, thread EventID) ([]byte, EventID, error) {
	if !validText(text) || len(metadata) > MaxTextLen {
		return nil, EventID{}, ErrBadBody
	}
	return l.Append(c, Draft{Kind: KindMsg, ReplyTo: replyTo, Thread: thread,
		Body: encodeBody(&MsgBody{Text: text, Type: typ, Metadata: metadata})})
}

// AppendEdit replaces the text of one of my messages.
func (l *Log) AppendEdit(c Conv, target EventID, text string) ([]byte, EventID, error) {
	if !validText(text) {
		return nil, EventID{}, ErrBadBody
	}
	return l.Append(c, Draft{Kind: KindEdit, Target: target, Body: encodeBody(&EditBody{Text: text})})
}

// AppendDelete deletes a message for everyone.
func (l *Log) AppendDelete(c Conv, target EventID) ([]byte, EventID, error) {
	return l.Append(c, Draft{Kind: KindDelete, Target: target})
}

// AppendReact sets or clears my reaction on a message.
func (l *Log) AppendReact(c Conv, target EventID, emoji string, on bool) ([]byte, EventID, error) {
	if !validEmoji(emoji) {
		return nil, EventID{}, ErrBadBody
	}
	return l.Append(c, Draft{Kind: KindReact, Target: target, Body: encodeBody(&ReactBody{Emoji: emoji, On: on})})
}

// AppendPin pins or unpins a message for the conversation.
func (l *Log) AppendPin(c Conv, target EventID, on bool) ([]byte, EventID, error) {
	return l.Append(c, Draft{Kind: KindPin, Target: target, Body: encodeBody(&PinBody{On: on})})
}

func (l *Log) render(bc, conv string, id string) (Message, bool) {
	k := viewKey(bc, id)
	if !l.st.Exists(k) {
		return Message{}, false
	}
	raw, err := l.st.Get(k)
	if err != nil {
		return Message{}, false
	}
	var v View
	if json.Unmarshal(raw, &v) != nil || !v.Present {
		return Message{}, false
	}
	v.ID = id
	return v.Render(conv, l.selfHex), true
}

// page reads one page of an index whose values are message ids.
func (l *Log) page(bc, conv string, prefix []byte, cursor string, limit int) ([]Message, string, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	start := prefix
	if cursor != "" {
		b, err := hex.DecodeString(cursor)
		if err != nil || !bytes.HasPrefix(b, prefix) {
			return nil, "", errors.New("bad cursor")
		}
		start = b
	}
	entries, err := l.st.FindValuesSeek(prefix, start, limit+1)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(entries) > limit {
		next = hex.EncodeToString(entries[limit].Key)
		entries = entries[:limit]
	}
	out := make([]Message, 0, len(entries))
	for _, e := range entries {
		if m, ok := l.render(bc, conv, string(e.Value)); ok {
			out = append(out, m)
		}
	}
	return out, next, nil
}

// History returns a page of a conversation, newest message first; pass the returned cursor to
// get the page before it ("" when there is none). Thread messages are not in it (see Thread).
func (l *Log) History(c Conv, cursor string, limit int) ([]Message, string, error) {
	bc := l.bc(c.ID)
	return l.page(bc, c.ID.String(), orderPrefix(bc), cursor, limit)
}

// Thread returns a page of the thread under root, oldest first.
func (l *Log) Thread(c Conv, root EventID, cursor string, limit int) ([]Message, string, error) {
	bc := l.bc(c.ID)
	return l.page(bc, c.ID.String(), threadPrefix(bc, root.String()), cursor, limit)
}

// Pins returns the pinned messages, most recently pinned first.
func (l *Log) Pins(c Conv) ([]Message, error) {
	bc := l.bc(c.ID)
	entries, err := l.st.FindValues(pinPrefix(bc), 0)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(entries))
	for _, e := range entries {
		if m, ok := l.render(bc, c.ID.String(), string(e.Value)); ok && m.Pinned {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PinnedAt > out[j].PinnedAt })
	return out, nil
}

// Message returns one message.
func (l *Log) Message(c Conv, id EventID) (Message, bool) {
	bc := l.bc(c.ID)
	return l.render(bc, c.ID.String(), id.String())
}

// Meta returns the state of a conversation.
func (l *Log) Meta(c Conv) (Meta, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bc := l.bc(c.ID)
	if !l.st.Exists(metaKey(bc)) {
		return Meta{}, false
	}
	return *l.loadMeta(bc, c), true
}

// Conversations lists every conversation, most recently active first.
func (l *Log) Conversations() ([]Meta, error) {
	entries, err := l.st.FindValues([]byte(pfxMeta), 0)
	if err != nil {
		return nil, err
	}
	out := make([]Meta, 0, len(entries))
	for _, e := range entries {
		var m Meta
		if json.Unmarshal(e.Value, &m) == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastTS != out[j].LastTS {
			return out[i].LastTS > out[j].LastTS
		}
		return out[i].Conv < out[j].Conv
	})
	return out, nil
}

// MarkRead records that the reader has seen the conversation up to message upTo.
func (l *Log) MarkRead(c Conv, upTo EventID) error {
	l.mu.Lock()
	bc := l.bc(c.ID)
	meta := l.loadMeta(bc, c)
	t := l.newTxn(c, bc, meta)
	v := t.view(upTo.String())
	if !v.Present {
		l.mu.Unlock()
		return ErrNotFound
	}
	if v.Lamport > meta.ReadLamport {
		meta.ReadLamport = v.Lamport
	}
	// everything above the read point in the main history is unread (my own messages move the
	// read point themselves)
	entries, err := l.st.FindValuesSeek(orderPrefix(bc), orderPrefix(bc), 10000)
	if err != nil {
		l.mu.Unlock()
		return err
	}
	n := 0
	p := len(orderPrefix(bc))
	for _, e := range entries {
		if len(e.Key) < p+8 {
			continue
		}
		lam := ^binary.BigEndian.Uint64(e.Key[p : p+8])
		if lam <= meta.ReadLamport {
			break
		}
		n++
	}
	meta.Unread = n
	t.views = map[string]*View{} // nothing about the message changed
	t.order = nil
	err = t.commit(nil, "")
	ch := &Change{Conv: c, Meta: *meta}
	l.mu.Unlock()
	if err == nil {
		l.notify(ch)
	}
	return err
}
