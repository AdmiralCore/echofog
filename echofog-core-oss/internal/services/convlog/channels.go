package convlog

import (
	"encoding/binary"
	"encoding/json"
	"sort"

	"google.golang.org/protobuf/proto"
)

// Channels of a group. A group (a space) is one log; its channels are named parts of it: a message
// says in Event.ext which channel it is in, and channel events (kind CHANNEL) create, rename and
// remove them. Membership, rights and sync stay those of the group's one log, so a channel needs no
// keys, no sync and no roster of its own. A voice room is a channel the app joins as a call. The
// main channel has the empty id and is every message without a channel (all the history from
// before channels).

// KindChannel creates, renames or removes a channel (body ChannelBody).
const KindChannel uint32 = 7

const (
	maxChannelID    = 32
	maxChannelName  = 64
	maxChannelTopic = 256
	maxChannels     = 200
)

// Channel is one channel of a group as the app shows it.
type Channel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Voice   bool   `json:"voice"`
	Removed  bool   `json:"removed,omitempty"`
	Topic    string `json:"topic,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"` // only those who may manage the group write in it
	Position int32  `json:"position"`           // the order among channels of its kind
	Reg      Reg    `json:"reg"`                // the last write that set it
	Created  uint64 `json:"created"`            // the clock of its earliest write: ties in order
}

// ChannelSpec is a channel as a manager wants it.
type ChannelSpec struct {
	ID, Name, Topic         string
	Voice, ReadOnly, Remove bool
	Position                int32
}

// ChanStat is where a channel's history ends and where its reader is.
type ChanStat struct {
	LastID      string `json:"last_id,omitempty"`
	LastLamport uint64 `json:"last_lamport,omitempty"`
	LastTS      int64  `json:"last_ts,omitempty"`
	Unread      int    `json:"unread,omitempty"`
	ReadLamport uint64 `json:"read_lamport,omitempty"`
}

func validChannelID(s string) bool {
	if s == "" || len(s) > maxChannelID {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// channelOf is the channel an event is in ("" for the main one, or when its ext is not readable).
func channelOf(e *Event) string {
	if len(e.Ext) == 0 {
		return ""
	}
	var x EventExt
	if proto.Unmarshal(e.Ext, &x) != nil || !validChannelID(x.Channel) {
		return ""
	}
	return x.Channel
}

func extFor(channel string) []byte {
	if channel == "" {
		return nil
	}
	return encodeBody(&EventExt{Channel: channel})
}

func (m *Meta) chan_(id string) *ChanStat {
	if m.Chans == nil {
		m.Chans = map[string]*ChanStat{}
	}
	s := m.Chans[id]
	if s == nil {
		s = &ChanStat{}
		m.Chans[id] = s
	}
	return s
}

// UnreadAll is the unread messages of a conversation in every channel.
func (m *Meta) UnreadAll() int {
	n := m.Unread
	for _, s := range m.Chans {
		n += s.Unread
	}
	return n
}

// applyChannel takes a channel event: who may change the group's channels may write it.
func (t *txn) applyChannel(e *Event, author string, rights Rights) error {
	if t.c.Kind != KindGroup || !rights.Manage {
		return ErrForbidden
	}
	var b ChannelBody
	if proto.Unmarshal(e.Body, &b) != nil || !validChannelID(b.Id) || len(b.Name) > maxChannelName || !validText(b.Name) ||
		(b.Name == "" && !b.Removed) || len(b.Topic) > maxChannelTopic || !validText(b.Topic) {
		return ErrBadBody
	}
	k := channelKey(t.bc, b.Id)
	cur, known := t.channel(b.Id)
	if !known {
		if n, err := t.l.st.FindValues(channelPrefix(t.bc), maxChannels+1); err == nil && len(n) >= maxChannels {
			return ErrForbidden
		}
	}
	reg := Reg{L: e.Lamport, A: author, S: e.Seq, TS: e.TsMs}
	created := e.Lamport
	if known && cur.Created < created {
		created = cur.Created
	}
	next := Channel{ID: b.Id, Name: b.Name, Voice: b.Voice, Removed: b.Removed, Topic: b.Topic, ReadOnly: b.ReadOnly,
		Position: b.Position, Reg: reg, Created: created}
	if known && !reg.after(cur.Reg) {
		next = cur // an older write: the channel keeps the newer one, and the earlier creation
		next.Created = created
	} else if b.Name == "" {
		next.Name = cur.Name
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	t.put(k, raw)
	if e.Lamport <= t.meta.MaxLamport {
		// a late change of who may write in it (read-only) weighs the messages around it again
		t.meta.Rebuild = true
	}
	return nil
}

// channel is a channel's state as this transaction sees it (written earlier in it, as a rebuild
// does, or stored).
func (t *txn) channel(id string) (Channel, bool) {
	k := channelKey(t.bc, id)
	var cur Channel
	known := false
	if t.l.st.Exists(k) {
		if raw, err := t.l.st.Get(k); err == nil && json.Unmarshal(raw, &cur) == nil {
			known = true
		}
	}
	for _, s := range t.sets {
		if string(s.Key) == string(k) {
			if json.Unmarshal(s.Value, &cur) == nil {
				known = true
			}
		}
	}
	return cur, known
}

// Channels lists a group's channels, oldest first, without the removed ones.
func (l *Log) Channels(c Conv) []Channel {
	bc := l.bc(c.ID)
	entries, err := l.st.FindValues(channelPrefix(bc), 0)
	if err != nil {
		return nil
	}
	out := make([]Channel, 0, len(entries))
	for _, en := range entries {
		var ch Channel
		if json.Unmarshal(en.Value, &ch) == nil && !ch.Removed {
			out = append(out, ch)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// AppendChannel writes a channel of a group as s says (a new id creates it).
func (l *Log) AppendChannel(c Conv, s ChannelSpec) ([]byte, EventID, error) {
	if !validChannelID(s.ID) || len(s.Name) > maxChannelName || !validText(s.Name) || (s.Name == "" && !s.Remove) ||
		len(s.Topic) > maxChannelTopic || !validText(s.Topic) {
		return nil, EventID{}, ErrBadBody
	}
	return l.Append(c, Draft{Kind: KindChannel, Body: encodeBody(&ChannelBody{Id: s.ID, Name: s.Name, Voice: s.Voice,
		Removed: s.Remove, Topic: s.Topic, ReadOnly: s.ReadOnly, Position: s.Position})})
}

// ChannelOf is one channel of a group (false: none such, or removed).
func (l *Log) ChannelOf(c Conv, id string) (Channel, bool) {
	for _, ch := range l.Channels(c) {
		if ch.ID == id {
			return ch, true
		}
	}
	return Channel{}, false
}

// AppendMessageIn writes a message to a channel of c ("" for the main one).
func (l *Log) AppendMessageIn(c Conv, channel, text string, typ uint32, metadata string, replyTo, thread EventID) ([]byte, EventID, error) {
	if channel != "" && (c.Kind != KindGroup || !validChannelID(channel)) {
		return nil, EventID{}, ErrBadBody
	}
	if !validText(text) || len(metadata) > MaxTextLen {
		return nil, EventID{}, ErrBadBody
	}
	return l.Append(c, Draft{Kind: KindMsg, ReplyTo: replyTo, Thread: thread, Ext: extFor(channel),
		Body: encodeBody(&MsgBody{Text: text, Type: typ, Metadata: metadata})})
}

// HistoryIn is History for one channel ("" for the main one).
func (l *Log) HistoryIn(c Conv, channel, cursor string, limit int) ([]Message, string, error) {
	if channel == "" {
		return l.History(c, cursor, limit)
	}
	bc := l.bc(c.ID)
	return l.page(bc, c.ID.String(), chanOrderPrefix(bc, channel), cursor, limit)
}

// MarkReadIn is MarkRead for one channel ("" for the main one).
func (l *Log) MarkReadIn(c Conv, channel string, upTo EventID) error {
	if channel == "" {
		return l.MarkRead(c, upTo)
	}
	l.mu.Lock()
	bc := l.bc(c.ID)
	meta := l.loadMeta(bc, c)
	t := l.newTxn(c, bc, meta)
	v := t.view(upTo.String())
	if !v.Present || v.Channel != channel {
		l.mu.Unlock()
		return ErrNotFound
	}
	st := meta.chan_(channel)
	if v.Lamport > st.ReadLamport {
		st.ReadLamport = v.Lamport
	}
	st.Unread = l.countAbove(chanOrderPrefix(bc, channel), st.ReadLamport, bc, meta.Conv)
	t.views, t.order = map[string]*View{}, nil
	err := t.commit(nil, "")
	ch := &Change{Conv: c, Meta: *meta, Rev: l.nextRev()}
	l.mu.Unlock()
	if err == nil {
		l.notify(ch)
	}
	return err
}

// countAbove counts the messages of others in an order index above the read point.
func (l *Log) countAbove(prefix []byte, read uint64, bc, conv string) int {
	entries, err := l.st.FindValuesSeek(prefix, prefix, 10000)
	if err != nil {
		return 0
	}
	n := 0
	p := len(prefix)
	for _, e := range entries {
		if len(e.Key) < p+8 {
			continue
		}
		if ^binary.BigEndian.Uint64(e.Key[p:p+8]) <= read {
			break
		}
		if m, ok := l.render(bc, conv, string(e.Value)); ok && !m.Mine {
			n++
		}
	}
	return n
}
