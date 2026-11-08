package convlog

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
)

// Files in a group and the closing of a group (T11.3e, conversation-log.md §2.5).

// KindClose is the event that closes a group (group.close).
const KindClose uint32 = 10

// ErrClosed reports an event refused because the group is closed.
var ErrClosed = errors.New("convlog: the group is closed")

const (
	attFidLen   = 16
	attRootLen  = 32
	maxAttName  = 255
	maxAttMime  = 127
	maxAttSize  = 1 << 50
	attScan     = 8     // the events of one conversation that may carry one file id
	maxCloseLen = 10000 // authors in a close vector
)

// AttRef is a file a group's log offers: the message that carries it and the Merkle root.
type AttRef struct {
	Msg  Message
	Root [32]byte
}

// checkAttachment reports whether an attachment is well formed: the sizes of the ids, a name that is a
// file name and not a path, a mime type of printable ASCII.
func checkAttachment(a *Attachment) error {
	switch {
	case a == nil, len(a.Fid) != attFidLen, len(a.MerkleRoot) != attRootLen, a.Size == 0, a.Size > maxAttSize:
		return ErrBadBody
	case a.Name == "", len(a.Name) > maxAttName, !utf8.ValidString(a.Name), a.Name == "." || a.Name == "..":
		return ErrBadBody
	case len(a.Mime) > maxAttMime:
		return ErrBadBody
	}
	for _, r := range a.Name {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return ErrBadBody
		}
	}
	for i := 0; i < len(a.Mime); i++ {
		if a.Mime[i] < 0x21 || a.Mime[i] > 0x7e {
			return ErrBadBody
		}
	}
	return nil
}

func attKey(bc, fid, id string) []byte { return bs(pfxAtt + bc + "/" + fid + "/" + id) }

// AppendAttachment writes a message that carries a file (at most one per event) to a channel of the
// group c, in a thread when thread is set. The file's bytes are not in the log: they come over files/2.
func (l *Log) AppendAttachment(c Conv, channel string, a *Attachment, text string, replyTo, thread EventID) ([]byte, EventID, error) {
	if c.Kind != KindGroup || checkAttachment(a) != nil || !validText(text) {
		return nil, EventID{}, ErrBadBody
	}
	if channel != "" && !validChannelID(channel) {
		return nil, EventID{}, ErrBadBody
	}
	return l.Append(c, Draft{Kind: KindMsg, ReplyTo: replyTo, Thread: thread, Ext: extFor(channel),
		Body: encodeBody(&MsgBody{Text: text, Att: a})})
}

// AttachmentIn finds the message of c that carries the file fid, if it is there, applied and not deleted.
// Whether its author may be heard and where the file belongs is for the caller to read from the message
// (author, channel, thread): never from anything the sender of an offer said.
func (l *Log) AttachmentIn(c Conv, fid [16]byte) (AttRef, bool) {
	if c.Kind != KindGroup {
		return AttRef{}, false
	}
	fh := hex.EncodeToString(fid[:])
	l.views.RLock()
	bc := l.bc(c.ID)
	prefix := bs(pfxAtt + bc + "/" + fh + "/")
	entries, err := l.st.FindValues(prefix, attScan)
	l.views.RUnlock()
	if err != nil {
		return AttRef{}, false
	}
	for _, en := range entries {
		id, err := ParseEventID(string(en.Value))
		if err != nil {
			continue
		}
		m, ok := l.Message(c, id)
		if !ok || m.Deleted || m.Withheld || m.File == nil || m.File.ID != fh {
			continue
		}
		root, err := hex.DecodeString(m.File.Root)
		if err != nil || len(root) != attRootLen {
			continue
		}
		return AttRef{Msg: m, Root: [32]byte(root)}, true
	}
	return AttRef{}, false
}

// ---- closing a group -------------------------------------------------------------------------------------------

// closeRec is the close the log accepted: its event id and the vector of the last seq per author.
type closeRec struct {
	ID   string            `json:"id"`
	Last map[string]uint64 `json:"last"` // author key (hex) -> last seq that counts
}

func (c *closeRec) allows(author []byte, seq uint64) bool {
	n, ok := c.Last[hex.EncodeToString(author)]
	return ok && seq <= n
}

func closeKey(bc string) []byte { return bs(pfxClose + bc) }

// closeOf is the close a group's log accepted, nil while it is open. Call with a lock held.
func (l *Log) closeOf(bc string) *closeRec {
	raw, err := l.st.Get(closeKey(bc))
	if err != nil {
		return nil
	}
	var c closeRec
	if json.Unmarshal(raw, &c) != nil || c.ID == "" {
		return nil
	}
	return &c
}

// decodeClose reads the vector of a close event: sorted by author, one entry each, keys of the right size.
func decodeClose(e *Event) (map[string]uint64, bool) {
	var b CloseBody
	if proto.Unmarshal(e.Body, &b) != nil || len(b.Last) > maxCloseLen {
		return nil, false
	}
	out := make(map[string]uint64, len(b.Last))
	var prev []byte
	for _, en := range b.Last {
		if len(en.Author) != authorLen || (prev != nil && bytes.Compare(prev, en.Author) >= 0) {
			return nil, false
		}
		prev = en.Author
		out[hex.EncodeToString(en.Author)] = en.Seq
	}
	return out, true
}

// applyClose takes a close event: the owner's, the first one, with a well formed vector. The close is
// irreversible; a repeated copy is the same event (the caller dedups by id), a different one is refused.
func (l *Log) applyClose(t *txn, e *Event, idHex string, at pos) error {
	if t.c.Kind != KindGroup {
		return ErrForbidden
	}
	role, _, ok := l.memberAt(t.c, t.bc, AuthorID(e), e.Author, at)
	if !ok || role.Rank < 100 {
		return ErrForbidden // only the owner closes a group
	}
	last, ok := decodeClose(e)
	if !ok {
		return ErrBadBody
	}
	if cur := l.closeOf(t.bc); cur != nil {
		if cur.ID != idHex {
			return ErrForbidden // a second, different close
		}
	} else {
		raw, _ := json.Marshal(closeRec{ID: idHex, Last: last})
		t.put(closeKey(t.bc), raw)
		// what was taken beyond the vector before the close came is weighed again
		limit := make(map[string]uint64, len(last))
		for k, seq := range last {
			if raw, err := hex.DecodeString(k); err == nil {
				if id, ok := pubToID(raw); ok {
					limit[peerHex(id)] = seq
				}
			}
		}
		for a, seq := range t.meta.Max {
			if lim, ok := limit[a]; !ok || seq > lim {
				t.meta.Rebuild = true
				break
			}
		}
	}
	t.meta.Closed = true
	return nil
}

type authorSeq struct {
	author []byte
	seq    uint64
}

// authorKeys lists the authors of a conversation by public key with the highest seq held of each. The
// version vector names authors by peer id; the key is read from one event of each. Call with l.mu held.
func (l *Log) authorKeys(c Conv, bc string) ([]authorSeq, error) {
	meta := l.loadMeta(bc, c)
	out := make([]authorSeq, 0, len(meta.Max))
	for a, seq := range meta.Max {
		if seq == 0 {
			continue
		}
		es, err := l.st.FindValues(bs(pfxEvent+bc+"/"+l.ba(a)+"/"), 1)
		if err != nil || len(es) == 0 {
			continue
		}
		e, _, err := Decode(es[0].Value)
		if err != nil {
			continue
		}
		out = append(out, authorSeq{author: e.Author, seq: seq})
	}
	return out, nil
}

// AppendClose closes the group c: only its owner may. The event carries the last seq the owner has seen per
// author; the group takes nothing beyond it afterwards.
func (l *Log) AppendClose(c Conv) ([]byte, EventID, error) {
	if c.Kind != KindGroup {
		return nil, EventID{}, ErrForbidden
	}
	l.mu.Lock()
	bc := l.bc(c.ID)
	if l.closeOf(bc) != nil {
		l.mu.Unlock()
		return nil, EventID{}, ErrClosed
	}
	keys, err := l.authorKeys(c, bc)
	l.mu.Unlock()
	if err != nil {
		return nil, EventID{}, err
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i].author, keys[j].author) < 0 })
	b := &CloseBody{}
	for _, k := range keys {
		b.Last = append(b.Last, &SeqEntry{Author: k.author, Seq: k.seq})
	}
	return l.Append(c, Draft{Kind: KindClose, Body: encodeBody(b)})
}

// Closed reports whether the group c is closed.
func (l *Log) Closed(c Conv) bool {
	if c.Kind != KindGroup {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeOf(l.bc(c.ID)) != nil
}
